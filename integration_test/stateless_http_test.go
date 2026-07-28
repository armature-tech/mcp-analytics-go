package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/armature-tech/mcp-analytics-go/armatureanalytics"
	armatureofficial "github.com/armature-tech/mcp-analytics-go/armatureanalytics/official"
	markmcp "github.com/mark3labs/mcp-go/mcp"
	markserver "github.com/mark3labs/mcp-go/server"
	officialmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type statelessEventCollector struct {
	mu     sync.Mutex
	events []armatureanalytics.Event
}

func (c *statelessEventCollector) emit(_ context.Context, batch armatureanalytics.Batch) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, batch.Events...)
	return nil
}

func (c *statelessEventCollector) snapshot() []armatureanalytics.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]armatureanalytics.Event(nil), c.events...)
}

type statelessOfficialInput struct {
	Text string `json:"text"`
}

type statelessOfficialOutput struct {
	Text string `json:"text"`
}

func parsedRequest(t *testing.T, r *http.Request) armatureanalytics.StatelessHTTPSession {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var body any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	return armatureanalytics.ResolveStatelessHTTPSession(armatureanalytics.StatelessHTTPInput{
		Body: body, Headers: r.Header,
	})
}

func statelessOfficialHandler(t *testing.T, collector *statelessEventCollector) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := parsedRequest(t, r)
		server, shutdown := armatureofficial.NewMCPServerWithConfig(
			&officialmcp.Implementation{Name: "official-stateless-e2e", Version: "1.0.0"},
			&officialmcp.ServerOptions{GetSessionID: session.SessionIDGenerator()},
			armatureanalytics.Config{Delivery: armatureanalytics.DeliveryAwait, Emit: collector.emit},
		)
		armatureofficial.InstrumentTool(server, &officialmcp.Tool{Name: "echo"},
			func(_ context.Context, _ *officialmcp.CallToolRequest, input statelessOfficialInput) (*officialmcp.CallToolResult, statelessOfficialOutput, error) {
				return nil, statelessOfficialOutput{Text: input.Text}, nil
			})
		handler := officialmcp.NewStreamableHTTPHandler(
			func(*http.Request) *officialmcp.Server { return server },
			&officialmcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
		)
		handler.ServeHTTP(w, r)
		_ = shutdown(context.Background())
	})
}

func statelessMark3labsHandler(t *testing.T, collector *statelessEventCollector) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := parsedRequest(t, r)
		server, shutdown := armatureanalytics.NewMCPServerWithConfig(
			"mark3labs-stateless-e2e",
			"1.0.0",
			armatureanalytics.Config{Delivery: armatureanalytics.DeliveryAwait, Emit: collector.emit},
			markserver.WithToolCapabilities(true),
		)
		armatureanalytics.InstrumentTool(server,
			markmcp.NewTool("echo", markmcp.WithString("text", markmcp.Required())),
			func(_ context.Context, req markmcp.CallToolRequest) (*markmcp.CallToolResult, error) {
				return markmcp.NewToolResultText(req.GetString("text", "")), nil
			},
		)
		handler := markserver.NewStreamableHTTPServer(
			server,
			markserver.WithSessionIdManager(session.Mark3labsSessionIDManager()),
			markserver.WithDisableStreaming(true),
		)
		handler.ServeHTTP(w, r)
		_ = shutdown(context.Background())
	})
}

// postRawJSONRPC posts one raw JSON-RPC message the way a legacy
// (pre-2026-07-28) HTTP client would, with optional extra headers.
func postRawJSONRPC(t *testing.T, ctx context.Context, url, body string, headers map[string]string) []byte {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("raw JSON-RPC call status %d: %s", response.StatusCode, payload)
	}
	return payload
}

// The official half of the stateless story changed with go-sdk v1.7.0: the
// v1.7.0 client speaks 2026-07-28 by default, which has no initialize and no
// Mcp-Session-Id, so the mint-on-initialize + echo scheme never engages.
// Client identity now arrives per request via `_meta`, and a request with no
// conversation signal correctly resolves to an empty session hint (server-side
// bucketing) instead of a fabricated one-request session.
func TestStatelessHTTPOfficialModernEra(t *testing.T) {
	collector := &statelessEventCollector{}
	httpServer := httptest.NewServer(statelessOfficialHandler(t, collector))
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := officialmcp.NewClient(
		&officialmcp.Implementation{Name: "parity-client", Version: "9.9.9"}, nil,
	)
	clientSession, err := client.Connect(ctx, &officialmcp.StreamableClientTransport{Endpoint: httpServer.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	if got := clientSession.InitializeResult().ProtocolVersion; got != "2026-07-28" {
		t.Fatalf("negotiated protocol = %q, want the modern era", got)
	}
	if _, err := clientSession.CallTool(ctx, &officialmcp.CallToolParams{
		Name: "echo", Arguments: map[string]any{"text": "stateless"},
	}); err != nil {
		t.Fatal(err)
	}

	var sawTool bool
	for _, event := range collector.snapshot() {
		if event.Kind != armatureanalytics.KindToolCall {
			continue
		}
		sawTool = true
		if event.SessionIDHint != nil {
			t.Fatalf("modern request without a conversation signal fabricated session %q", *event.SessionIDHint)
		}
		if event.Metadata["client_name"] != "parity-client" || event.Metadata["client_version"] != "9.9.9" {
			t.Fatalf("missing per-request client identity: %#v", event.Metadata)
		}
	}
	if !sawTool {
		t.Fatalf("missing tool_call event in %#v", collector.snapshot())
	}
}

// The mark3labs framework has no 2026-07-28 support, so its stateless flow is
// unchanged: the v1.7.0 client's server/discover probe fails there and falls
// back to the legacy initialize handshake, which still mints an
// identity-bearing session that the client echoes on later requests.
func TestStatelessHTTPMark3labsLegacyFlow(t *testing.T) {
	collector := &statelessEventCollector{}
	httpServer := httptest.NewServer(statelessMark3labsHandler(t, collector))
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := officialmcp.NewClient(
		&officialmcp.Implementation{Name: "parity-client", Version: "9.9.9"}, nil,
	)
	clientSession, err := client.Connect(ctx, &officialmcp.StreamableClientTransport{Endpoint: httpServer.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	if _, err := clientSession.CallTool(ctx, &officialmcp.CallToolParams{
		Name: "echo", Arguments: map[string]any{"text": "stateless"},
	}); err != nil {
		t.Fatal(err)
	}

	events := collector.snapshot()
	var sessionID string
	var sawTool, sawClient bool
	for _, event := range events {
		if event.SessionIDHint == nil || *event.SessionIDHint == "" {
			t.Fatalf("event lost stateless session identity: %#v", event)
		}
		if sessionID == "" {
			sessionID = *event.SessionIDHint
		} else if *event.SessionIDHint != sessionID {
			t.Fatalf("events split across %q and %q", sessionID, *event.SessionIDHint)
		}
		if event.Kind == armatureanalytics.KindToolCall {
			sawTool = true
		}
		if event.Metadata["client_name"] == "parity-client" && event.Metadata["client_version"] == "9.9.9" {
			sawClient = true
		}
	}
	if !sawTool || !sawClient {
		t.Fatalf("missing tool/client evidence in %#v", events)
	}

	// A tools/call that lost its echoed Mcp-Session-Id no longer receives a
	// fabricated one-request session: it flows through with an empty hint and
	// ingest buckets it server-side.
	postRawJSONRPC(t, ctx, httpServer.URL,
		`{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"echo","arguments":{"text":"no echo"}}}`, nil)
	var sawFallback bool
	for _, event := range collector.snapshot() {
		if event.Kind != armatureanalytics.KindToolCall {
			continue
		}
		if event.SessionIDHint == nil {
			sawFallback = true
		} else if *event.SessionIDHint != sessionID {
			t.Fatalf("missing-echo call fabricated session %q", *event.SessionIDHint)
		}
	}
	if !sawFallback {
		t.Fatal("missing-echo tool call did not flow through with an empty session hint")
	}
}
