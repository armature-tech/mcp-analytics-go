package integration_test

// Mirrors the 2026-07-29 live repro against go-sdk v1.7.0
// StreamableHTTPHandler{Stateless: true}: a legacy-era client (real Claude
// Code 2.1.x) gets no Mcp-Session-Id — GetSessionID is never consulted — so
// its tool_call events land in a fallback bucket with no client identity
// while the initialize's session_init lands in a separate one. The
// WrapStatelessHTTPHandler middleware repairs exactly that, and modern-era
// (2026-07-28 `_meta` envelope) requests pass through it untouched.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/armature-tech/mcp-analytics-go/armatureanalytics"
	armatureofficial "github.com/armature-tech/mcp-analytics-go/armatureanalytics/official"
	officialmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// wrappedStatelessOfficialServer is the live repro's server shape: a
// per-request official v1.7.0 stateless server, now wrapped with the repair
// middleware sharing the same config. The echo tool returns the session the
// middleware resolved into the request context.
func wrappedStatelessOfficialServer(t *testing.T, collector *statelessEventCollector) *httptest.Server {
	t.Helper()
	cfg := armatureanalytics.Config{Delivery: armatureanalytics.DeliveryAwait, Emit: collector.emit}
	rec, err := armatureanalytics.NewRecorder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, _ := armatureanalytics.StatelessHTTPSessionFromRequest(r)
		server, shutdown := armatureofficial.NewMCPServerWithConfig(
			&officialmcp.Implementation{Name: "official-stateless-repair-e2e", Version: "1.0.0"},
			nil,
			cfg,
		)
		armatureofficial.InstrumentTool(server, &officialmcp.Tool{Name: "echo"},
			func(_ context.Context, _ *officialmcp.CallToolRequest, input statelessOfficialInput) (*officialmcp.CallToolResult, statelessOfficialOutput, error) {
				return nil, statelessOfficialOutput{Text: input.Text + "|" + session.SessionID}, nil
			})
		handler := officialmcp.NewStreamableHTTPHandler(
			func(*http.Request) *officialmcp.Server { return server },
			&officialmcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
		)
		handler.ServeHTTP(w, r)
		_ = shutdown(context.Background())
	})
	return httptest.NewServer(rec.WrapStatelessHTTPHandler(inner))
}

// postLegacyJSONRPC posts one raw JSON-RPC message the way a pre-2026-07-28
// client does and returns the decoded payload plus response headers.
func postLegacyJSONRPC(t *testing.T, ctx context.Context, url, body string, headers map[string]string) ([]byte, http.Header) {
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
		t.Fatalf("legacy JSON-RPC call status %d: %s", response.StatusCode, payload)
	}
	return payload, response.Header
}

func TestWrapStatelessHTTPHandlerRepairsLegacyClient(t *testing.T) {
	collector := &statelessEventCollector{}
	httpServer := wrappedStatelessOfficialServer(t, collector)
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Request shape 1: the legacy handshake, exactly what Claude Code 2.1.x
	// sends. v1.7.0 stateless alone answers it with no session header.
	_, headers := postLegacyJSONRPC(t, ctx, httpServer.URL,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude-code","version":"2.1.36"}}}`,
		nil)
	minted := headers.Get("Mcp-Session-Id")
	if minted == "" {
		t.Fatal("wrapped legacy initialize did not mint an Mcp-Session-Id")
	}
	info := armatureanalytics.ParseStatelessSessionClientInfo(minted)
	if info == nil || info.Name != "claude-code" || info.Version != "2.1.36" {
		t.Fatalf("minted id %q does not carry the client identity", minted)
	}

	echo := map[string]string{"Mcp-Session-Id": minted}
	postLegacyJSONRPC(t, ctx, httpServer.URL,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`, echo)

	// Request shape 2: the legacy tools/call carrying only the echoed header.
	payload, callHeaders := postLegacyJSONRPC(t, ctx, httpServer.URL,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"legacy"}}}`, echo)
	if got := callHeaders.Get("Mcp-Session-Id"); got != minted {
		t.Fatalf("tools/call response header = %q, want the echoed id %q", got, minted)
	}
	// The middleware's context propagation reaches the per-request tool
	// handler: the tool saw the same session the header carries.
	if !bytes.Contains(payload, []byte("legacy|"+minted)) {
		t.Fatalf("tool did not observe the middleware-resolved session: %s", payload)
	}

	var sessionInitIDs []string
	var sawToolCall bool
	for _, event := range collector.snapshot() {
		switch event.Kind {
		case armatureanalytics.KindSessionInit:
			if event.SessionIDHint == nil || *event.SessionIDHint != minted {
				t.Fatalf("session_init outside the minted session: %#v", event)
			}
			if event.Metadata["client_name"] != "claude-code" {
				t.Fatalf("session_init lost client identity: %#v", event.Metadata)
			}
			sessionInitIDs = append(sessionInitIDs, event.EventID)
		case armatureanalytics.KindToolCall:
			sawToolCall = true
			if event.SessionIDHint == nil || *event.SessionIDHint != minted {
				t.Fatalf("tool_call lost the minted session (the live-bug fallback bucket): %#v", event)
			}
			if event.Metadata["client_name"] != "claude-code" || event.Metadata["client_version"] != "2.1.36" {
				t.Fatalf("tool_call lost client identity: %#v", event.Metadata)
			}
		}
	}
	if !sawToolCall || len(sessionInitIDs) == 0 {
		t.Fatalf("missing tool_call/session_init evidence in %#v", collector.snapshot())
	}
	// The middleware and the per-request server may both record the
	// session_init; the deterministic event id makes ingest converge them.
	for _, id := range sessionInitIDs {
		if id != sessionInitIDs[0] {
			t.Fatalf("session_init event ids diverge (%q vs %q): ingest would keep both", id, sessionInitIDs[0])
		}
	}
}

// The canary flow: every request carries X-Armature-Session-Seed (the
// workflow-run id), so the mint embeds the seed uuid. The seed rung must then
// yield the richer minted id on echoed requests — not the bare seed — or one
// legacy conversation splits across two session hints.
func TestWrapStatelessHTTPHandlerSeededLegacyConversation(t *testing.T) {
	collector := &statelessEventCollector{}
	httpServer := wrappedStatelessOfficialServer(t, collector)
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const seed = "0f1e2d3c-4b5a-4978-8695-a4b3c2d1e0f9"
	headers := map[string]string{"X-Armature-Session-Seed": seed}
	_, initHeaders := postLegacyJSONRPC(t, ctx, httpServer.URL,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude-code","version":"2.1.36"}}}`,
		headers)
	minted := initHeaders.Get("Mcp-Session-Id")
	if !strings.HasSuffix(minted, "_"+seed) {
		t.Fatalf("minted id %q does not embed the seed %q", minted, seed)
	}
	echo := map[string]string{"X-Armature-Session-Seed": seed, "Mcp-Session-Id": minted}
	postLegacyJSONRPC(t, ctx, httpServer.URL,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"seeded"}}}`, echo)

	for _, event := range collector.snapshot() {
		if event.Kind != armatureanalytics.KindSessionInit && event.Kind != armatureanalytics.KindToolCall {
			continue
		}
		if event.SessionIDHint == nil || *event.SessionIDHint != minted {
			t.Fatalf("%s split off the minted session (hint %v, want %q)", event.Kind, event.SessionIDHint, minted)
		}
	}
}

func TestWrapStatelessHTTPHandlerLeavesModernEraUntouched(t *testing.T) {
	collector := &statelessEventCollector{}
	httpServer := wrappedStatelessOfficialServer(t, collector)
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The real v1.7.0 client negotiates 2026-07-28 through the wrapped
	// handler exactly as it does unwrapped.
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
		Name: "echo", Arguments: map[string]any{"text": "modern"},
	}); err != nil {
		t.Fatal(err)
	}

	// A raw modern-era request (per-request `_meta` envelope, no initialize)
	// must come back without any session header fabricated by the middleware.
	meta := fmt.Sprintf(`{"%s":"2026-07-28","%s":{"name":"claude-code","version":"3.0.0"},"%s":{}}`,
		officialmcp.MetaKeyProtocolVersion, officialmcp.MetaKeyClientInfo, officialmcp.MetaKeyClientCapabilities)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL, strings.NewReader(
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"echo","arguments":{"text":"raw-modern"},"_meta":`+meta+`}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", "echo")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("raw modern tools/call status %d: %s", response.StatusCode, body)
	}
	if got := response.Header.Get("Mcp-Session-Id"); got != "" {
		t.Fatalf("middleware minted %q for a modern-era request", got)
	}

	var toolCalls int
	for _, event := range collector.snapshot() {
		if event.Kind != armatureanalytics.KindToolCall {
			continue
		}
		toolCalls++
		if event.SessionIDHint != nil {
			t.Fatalf("modern request without a conversation signal fabricated session %q", *event.SessionIDHint)
		}
		if name := event.Metadata["client_name"]; name != "parity-client" && name != "claude-code" {
			t.Fatalf("missing per-request client identity: %#v", event.Metadata)
		}
	}
	if toolCalls < 2 {
		t.Fatalf("expected both modern tool calls recorded, got %d", toolCalls)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("raw modern response is not JSON: %s", body)
	}
}
