package integration_test

// End-to-end coverage for MCP 2026-07-28 ("modern era") against go-sdk
// v1.7.0: one instrumented mcp.Server behind a
// StreamableHTTPHandler{Stateless: true}, driven by the v1.7.0 client (which
// speaks 2026-07-28 by default via server/discover) and by raw JSON-RPC POSTs
// shaped like pre-2026-07-28 legacy clients. The same handler serves both
// eras.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/armature-tech/mcp-analytics-go/armatureanalytics"
	armatureofficial "github.com/armature-tech/mcp-analytics-go/armatureanalytics/official"
	officialmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type modernEchoInput struct {
	Text string `json:"text"`
}

type modernEchoOutput struct {
	Text string `json:"text"`
}

// newModernStatelessServer builds one long-lived instrumented server (the
// non-serverless topology: a single recorder observes every request) behind a
// v1.7.0 stateless streamable handler.
func newModernStatelessServer(t *testing.T) (*httptest.Server, *statelessEventCollector) {
	t.Helper()
	collector := &statelessEventCollector{}
	server, shutdown := armatureofficial.NewMCPServerWithConfig(
		&officialmcp.Implementation{Name: "modern-stateless", Version: "1.0.0"},
		nil,
		armatureanalytics.Config{
			Delivery:          armatureanalytics.DeliveryAwait,
			Emit:              collector.emit,
			RequestCapability: boolPtr(false),
		},
	)
	// Typed tool via InstrumentTool: v1.7.0 validates tool schemas (SEP-2106)
	// at registration and validates call arguments against them, so this
	// round-trip proves the telemetry-decorated schema still passes.
	armatureofficial.InstrumentTool(server, &officialmcp.Tool{Name: "echo", Description: "Echo a value"},
		func(_ context.Context, _ *officialmcp.CallToolRequest, input modernEchoInput) (*officialmcp.CallToolResult, modernEchoOutput, error) {
			return nil, modernEchoOutput{Text: input.Text}, nil
		})
	handler := officialmcp.NewStreamableHTTPHandler(
		func(*http.Request) *officialmcp.Server { return server },
		&officialmcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(func() {
		httpServer.Close()
		_ = shutdown(context.Background())
	})
	return httpServer, collector
}

// headerRoundTripper stamps fixed headers onto every outgoing request, the
// way the Armature remote proxy forwards X-Armature-Session-Seed.
type headerRoundTripper struct {
	headers map[string]string
}

func (h headerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	for key, value := range h.headers {
		clone.Header.Set(key, value)
	}
	return http.DefaultTransport.RoundTrip(clone)
}

func connectModernClient(t *testing.T, ctx context.Context, endpoint string, headers map[string]string) *officialmcp.ClientSession {
	t.Helper()
	client := officialmcp.NewClient(&officialmcp.Implementation{Name: "modern-client", Version: "3.1.4"}, nil)
	transport := &officialmcp.StreamableClientTransport{Endpoint: endpoint}
	if len(headers) > 0 {
		transport.HTTPClient = &http.Client{Transport: headerRoundTripper{headers: headers}}
	}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if got := session.InitializeResult().ProtocolVersion; got != "2026-07-28" {
		t.Fatalf("negotiated protocol = %q, want 2026-07-28", got)
	}
	return session
}

func toolCallEvents(events []armatureanalytics.Event) []armatureanalytics.Event {
	var out []armatureanalytics.Event
	for _, event := range events {
		if event.Kind == armatureanalytics.KindToolCall {
			out = append(out, event)
		}
	}
	return out
}

// hintOf returns the session hint of the tool_call whose input preview
// contains marker, plus whether the event was found.
func hintOf(events []armatureanalytics.Event, marker string) (*string, bool) {
	for _, event := range events {
		preview, _ := event.Metadata["input_preview"].(string)
		if strings.Contains(preview, marker) {
			return event.SessionIDHint, true
		}
	}
	return nil, false
}

func TestModernEraToolCallsAndSessionInit(t *testing.T) {
	httpServer, collector := newModernStatelessServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session := connectModernClient(t, ctx, httpServer.URL, nil)

	// gen_ai.conversation.id travels in the fixed `_meta` "baggage" trace
	// slot; the value is percent-encoded per W3C Baggage.
	baggage := officialmcp.Meta{"baggage": "app=demo;prop=1,gen_ai.conversation.id=conv%2Dmodern%2D1"}
	for _, text := range []string{"first-call", "second-call"} {
		result, err := session.CallTool(ctx, &officialmcp.CallToolParams{
			Meta: baggage,
			Name: "echo",
			Arguments: map[string]any{
				"text":      text,
				"telemetry": map[string]any{"user_intent": "exercise the modern era"},
			},
		})
		if err != nil || result.IsError {
			t.Fatalf("tools/call %q: result=%#v err=%v", text, result, err)
		}
	}

	events := collector.snapshot()
	calls := toolCallEvents(events)
	if len(calls) != 2 {
		t.Fatalf("tool_call events = %d, want 2: %#v", len(calls), events)
	}
	eventIDRE := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, call := range calls {
		// Wire-contract shape (TELEMETRY-CONTRACT.md is unchanged by the
		// protocol bump).
		if !eventIDRE.MatchString(call.EventID) || !eventIDRE.MatchString(call.ActorID) {
			t.Fatalf("malformed event/actor id: %#v", call)
		}
		if _, err := time.Parse(time.RFC3339Nano, call.StartedAt); err != nil {
			t.Fatalf("started_at not RFC3339: %v", err)
		}
		if call.Calls == nil || call.Logs == nil || call.SearchCalls == nil {
			t.Fatalf("nil contract arrays: %#v", call)
		}
		if call.SessionIDHint == nil || *call.SessionIDHint != "conv-modern-1" {
			t.Fatalf("session hint = %v, want baggage conversation id", call.SessionIDHint)
		}
		if call.Metadata["tool_name"] != "echo" {
			t.Fatalf("tool_name = %#v", call.Metadata["tool_name"])
		}
		if call.Metadata["client_name"] != "modern-client" || call.Metadata["client_version"] != "3.1.4" {
			t.Fatalf("per-request client identity missing: %#v", call.Metadata)
		}
		if call.Metadata["protocol_version"] != "2026-07-28" {
			t.Fatalf("protocol_version = %#v", call.Metadata["protocol_version"])
		}
		if call.Metadata["user_intent"] != "exercise the modern era" {
			t.Fatalf("user_intent = %#v", call.Metadata["user_intent"])
		}
		preview, _ := call.Metadata["input_preview"].(string)
		if strings.Contains(preview, "telemetry") {
			t.Fatalf("telemetry leaked into input preview: %s", preview)
		}
		requestMeta, ok := call.Metadata["request_meta"].(map[string]any)
		if !ok {
			t.Fatalf("request_meta missing: %#v", call.Metadata["request_meta"])
		}
		for _, key := range []string{
			"io.modelcontextprotocol/protocolVersion",
			"io.modelcontextprotocol/clientCapabilities",
			"io.modelcontextprotocol/clientInfo",
			"baggage",
		} {
			if _, present := requestMeta[key]; !present {
				t.Fatalf("request_meta lost %q: %#v", key, requestMeta)
			}
		}
		if truncated, present := call.Metadata["request_meta_truncated"]; present {
			t.Fatalf("small request_meta flagged truncated: %v", truncated)
		}
	}

	// session_init exactly once per session key, with the per-request client
	// identity, despite two tool calls (and no initialize handshake at all).
	var inits []armatureanalytics.Event
	for _, event := range events {
		if event.Kind == armatureanalytics.KindSessionInit {
			inits = append(inits, event)
		}
	}
	if len(inits) != 1 {
		t.Fatalf("session_init events = %d, want exactly 1: %#v", len(inits), inits)
	}
	if inits[0].SessionIDHint == nil || *inits[0].SessionIDHint != "conv-modern-1" {
		t.Fatalf("session_init hint = %v", inits[0].SessionIDHint)
	}
	if inits[0].Metadata["client_name"] != "modern-client" {
		t.Fatalf("session_init client = %#v", inits[0].Metadata)
	}
}

func TestModernEraSessionLadderPriority(t *testing.T) {
	httpServer, collector := newModernStatelessServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	seed := "11111111-2222-4333-8444-555555555555"
	session := connectModernClient(t, ctx, httpServer.URL, map[string]string{"X-Armature-Session-Seed": seed})

	// (1) baggage beats the seed header.
	if _, err := session.CallTool(ctx, &officialmcp.CallToolParams{
		Meta:      officialmcp.Meta{"baggage": "gen_ai.conversation.id=conv-over-seed"},
		Name:      "echo",
		Arguments: map[string]any{"text": "ladder-baggage"},
	}); err != nil {
		t.Fatal(err)
	}
	// (2) the seed header when no baggage conversation id exists.
	if _, err := session.CallTool(ctx, &officialmcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"text": "ladder-seed"},
	}); err != nil {
		t.Fatal(err)
	}
	// (2 beats 3) legacy-era request carrying both a seed and an echoed
	// Mcp-Session-Id: the seed wins.
	legacyID := armatureanalytics.BuildStatelessSessionID(&armatureanalytics.ClientInfo{Name: "legacy-echo-client", Version: "1.2"})
	postRawJSONRPC(t, ctx, httpServer.URL,
		`{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"echo","arguments":{"text":"ladder-seed-beats-legacy"}}}`,
		map[string]string{"X-Armature-Session-Seed": seed, "Mcp-Session-Id": legacyID})
	// (3) the legacy id alone still resolves.
	postRawJSONRPC(t, ctx, httpServer.URL,
		`{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"echo","arguments":{"text":"ladder-legacy"}}}`,
		map[string]string{"Mcp-Session-Id": legacyID})

	events := collector.snapshot()
	expect := map[string]string{
		"ladder-baggage":           "conv-over-seed",
		"ladder-seed":              seed,
		"ladder-seed-beats-legacy": seed,
		"ladder-legacy":            legacyID,
	}
	for marker, want := range expect {
		hint, found := hintOf(events, marker)
		if !found {
			t.Fatalf("no tool_call for %q in %#v", marker, events)
		}
		if hint == nil || *hint != want {
			t.Fatalf("%q session hint = %v, want %q", marker, hint, want)
		}
	}
	// The legacy identity-bearing session id still recovers client identity.
	for _, call := range toolCallEvents(events) {
		preview, _ := call.Metadata["input_preview"].(string)
		if strings.Contains(preview, "ladder-legacy") && call.Metadata["client_name"] != "legacy-echo-client" {
			t.Fatalf("echoed legacy id lost client identity: %#v", call.Metadata)
		}
	}
}

// A v1.7.0 stateless handler still serves pre-2026-07-28 clients: initialize
// keeps working as the legacy session_init signal, on the same handler that
// serves modern traffic.
func TestLegacyEraThroughModernStatelessHandler(t *testing.T) {
	httpServer, collector := newModernStatelessServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	response := postRawJSONRPC(t, ctx, httpServer.URL,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"legacy-raw-client","version":"0.9"},"capabilities":{}}}`,
		nil)
	if !strings.Contains(string(response), "2025-11-25") {
		t.Fatalf("legacy initialize was not answered in kind: %s", response)
	}
	postRawJSONRPC(t, ctx, httpServer.URL,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"legacy-era-call"}}}`,
		nil)

	events := collector.snapshot()
	var sawInit bool
	for _, event := range events {
		if event.Kind == armatureanalytics.KindSessionInit && event.Metadata["client_name"] == "legacy-raw-client" {
			sawInit = true
			if event.Metadata["protocol_version"] != "2025-11-25" {
				t.Fatalf("legacy session_init protocol = %#v", event.Metadata["protocol_version"])
			}
		}
	}
	if !sawInit {
		t.Fatalf("no legacy session_init in %#v", events)
	}
	if _, found := hintOf(events, "legacy-era-call"); !found {
		t.Fatalf("legacy tools/call not recorded: %#v", events)
	}
}

// clientInfo is OPTIONAL per request in 2026-07-28 (the required pair is
// protocolVersion + clientCapabilities). Such requests record an unknown
// client rather than crashing or being dropped.
func TestModernEraClientInfoAbsent(t *testing.T) {
	httpServer, collector := newModernStatelessServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	body := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}},` +
		`"name":"echo","arguments":{"text":"anonymous-modern"}}}`
	// SEP-2243: modern-era HTTP requests must mirror the JSON-RPC envelope in
	// standard headers (Mcp-Protocol-Version, Mcp-Method, and Mcp-Name for
	// tools/call) or v1.7.0 answers -32020 HeaderMismatch.
	response := postRawJSONRPC(t, ctx, httpServer.URL, body, map[string]string{
		"Mcp-Protocol-Version": "2026-07-28",
		"Mcp-Method":           "tools/call",
		"Mcp-Name":             "echo",
	})
	var decoded map[string]any
	if err := json.Unmarshal(response, &decoded); err == nil {
		if _, isError := decoded["error"]; isError {
			t.Fatalf("clientInfo-less modern call rejected: %s", response)
		}
	}

	events := collector.snapshot()
	for _, call := range toolCallEvents(events) {
		preview, _ := call.Metadata["input_preview"].(string)
		if !strings.Contains(preview, "anonymous-modern") {
			continue
		}
		if name, present := call.Metadata["client_name"]; present && name != nil {
			t.Fatalf("client identity fabricated: %#v", call.Metadata)
		}
		if call.Metadata["protocol_version"] != "2026-07-28" {
			t.Fatalf("protocol_version = %#v", call.Metadata["protocol_version"])
		}
		if call.SessionIDHint != nil {
			t.Fatalf("sessionless modern request fabricated hint %q", *call.SessionIDHint)
		}
		return
	}
	t.Fatalf("no tool_call for the clientInfo-less request: %#v", events)
}
