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

func TestStatelessHTTPFeatureParityAcrossGoFrameworks(t *testing.T) {
	tests := []struct {
		name    string
		handler func(*testing.T, *statelessEventCollector) http.Handler
	}{
		{name: "official", handler: statelessOfficialHandler},
		{name: "mark3labs", handler: statelessMark3labsHandler},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			collector := &statelessEventCollector{}
			httpServer := httptest.NewServer(test.handler(t, collector))
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

			fallbackRequest, err := http.NewRequestWithContext(
				ctx,
				http.MethodPost,
				httpServer.URL,
				bytes.NewBufferString(`{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"echo","arguments":{"text":"no echo"}}}`),
			)
			if err != nil {
				t.Fatal(err)
			}
			fallbackRequest.Header.Set("Content-Type", "application/json")
			fallbackRequest.Header.Set("Accept", "application/json, text/event-stream")
			fallbackResponse, err := http.DefaultClient.Do(fallbackRequest)
			if err != nil {
				t.Fatal(err)
			}
			fallbackBody, _ := io.ReadAll(fallbackResponse.Body)
			_ = fallbackResponse.Body.Close()
			if fallbackResponse.StatusCode < 200 || fallbackResponse.StatusCode >= 300 {
				t.Fatalf("missing-echo call status %d: %s", fallbackResponse.StatusCode, fallbackBody)
			}

			var fallbackSessionID string
			for _, event := range collector.snapshot() {
				if event.Kind == armatureanalytics.KindToolCall && event.SessionIDHint != nil && *event.SessionIDHint != sessionID {
					fallbackSessionID = *event.SessionIDHint
				}
			}
			if fallbackSessionID == "" {
				t.Fatal("missing-echo tool call lost its one-off analytics session")
			}
		})
	}
}
