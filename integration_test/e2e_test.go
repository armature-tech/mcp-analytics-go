// Package integration_test exercises the armatureanalytics recorder against
// a real in-process mark3labs/mcp-go server. The test drives a tool call
// through the full hook chain and verifies the resulting Armature ingest
// payloads.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/armature-tech/mcp-analytics-go/armatureanalytics"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func boolPtr(v bool) *bool { return &v }

type ingestSink struct {
	mu     sync.Mutex
	events []map[string]any
	server *httptest.Server
}

func newIngestSink(t *testing.T) *ingestSink {
	s := &ingestSink{}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.server.Close)
	return s
}

func (s *ingestSink) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var batch struct {
		SchemaVersion int              `json:"schema_version"`
		Events        []map[string]any `json:"events"`
	}
	_ = json.Unmarshal(body, &batch)
	s.mu.Lock()
	s.events = append(s.events, batch.Events...)
	s.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func (s *ingestSink) Events() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, len(s.events))
	copy(out, s.events)
	return out
}

func TestRecorder_EndToEnd_ToolCallAndSessionInit(t *testing.T) {
	sink := newIngestSink(t)

	rec, err := armatureanalytics.NewRecorder(armatureanalytics.Config{
		APIKey:      "test-key",
		EndpointURL: sink.server.URL,
		Timeout:     2 * time.Second,
		ActorSeed:   func(_ context.Context) string { return "user-42" },
	})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	mcpServer := server.NewMCPServer(
		"test-server", "1.0.0",
		server.WithToolCapabilities(true),
		server.WithHooks(rec.Hooks()),
	)
	mcpServer.AddTool(
		mcp.NewTool("echo",
			mcp.WithDescription("Echoes its input"),
			mcp.WithString("text", mcp.Description("Text to echo")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				mcp.TextContent{Type: "text", Text: "echoed: " + req.GetArguments()["text"].(string)},
			}}, nil
		},
	)
	mcpServer.AddTool(
		mcp.NewTool("explode", mcp.WithDescription("Always fails")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, errors.New("boom")
		},
	)

	client, err := mcpclient.NewInProcessClient(mcpServer)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("client.Start: %v", err)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = "2025-06-18"
	initReq.Params.ClientInfo.Name = "test-client"
	initReq.Params.ClientInfo.Version = "0.1"
	if _, err := client.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	// Successful tool call.
	callReq := mcp.CallToolRequest{}
	callReq.Params.Name = "echo"
	callReq.Params.Arguments = map[string]any{"text": "hi"}
	if _, err := client.CallTool(ctx, callReq); err != nil {
		t.Fatalf("CallTool echo: %v", err)
	}

	// Failing tool call.
	failReq := mcp.CallToolRequest{}
	failReq.Params.Name = "explode"
	if _, err := client.CallTool(ctx, failReq); err == nil {
		// mcp-go may surface the handler error as a JSON-RPC error OR
		// as an IsError=true result depending on routing. Either path is
		// valid; the assertion is that the recorder emits an !ok event.
		t.Logf("explode returned no error (expected for IsError-style failure)")
	}

	if err := rec.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	events := sink.Events()
	if len(events) < 2 {
		t.Fatalf("got %d events, want at least 2 (session_init + tool_calls). events=%v", len(events), events)
	}

	var (
		sawSessionInit bool
		sawEchoOK      bool
		sawExplodeErr  bool
	)
	for _, ev := range events {
		switch ev["kind"] {
		case "session_init":
			sawSessionInit = true
			if meta, _ := ev["metadata"].(map[string]any); meta["client_name"] != "test-client" {
				t.Errorf("session_init client_name = %v, want test-client", meta["client_name"])
			}
		case "tool_call":
			meta, _ := ev["metadata"].(map[string]any)
			switch meta["tool_name"] {
			case "echo":
				sawEchoOK = ev["ok"] == true
			case "explode":
				if ev["ok"] == false {
					sawExplodeErr = true
				}
			}
		}
	}
	if !sawSessionInit {
		t.Errorf("missing session_init event")
	}
	if !sawEchoOK {
		t.Errorf("missing successful echo tool_call event")
	}
	if !sawExplodeErr {
		t.Errorf("missing failed explode tool_call event")
	}
}

func TestRecorder_DisabledIsNoop(t *testing.T) {
	sink := newIngestSink(t)
	rec, err := armatureanalytics.NewRecorder(armatureanalytics.Config{
		APIKey:      "test-key",
		EndpointURL: sink.server.URL,
		Disabled:    true,
	})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	mcpServer := server.NewMCPServer("s", "1.0", server.WithToolCapabilities(true), server.WithHooks(rec.Hooks()))
	mcpServer.AddTool(
		mcp.NewTool("ping"),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "pong"}}}, nil
		},
	)
	client, _ := mcpclient.NewInProcessClient(mcpServer)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = client.Start(ctx)
	_, _ = client.Initialize(ctx, mcp.InitializeRequest{})
	req := mcp.CallToolRequest{}
	req.Params.Name = "ping"
	_, _ = client.CallTool(ctx, req)
	_ = rec.Flush(context.Background())

	if got := sink.Events(); len(got) != 0 {
		t.Fatalf("disabled recorder emitted %d events; want 0", len(got))
	}
}

func TestRequestCapability_OptInEmitsNormalToolCall(t *testing.T) {
	sink := newIngestSink(t)
	mcpServer, shutdown := armatureanalytics.NewMCPServerWithConfig(
		"test-server",
		"1.0.0",
		armatureanalytics.Config{
			APIKey:            "test-key",
			EndpointURL:       sink.server.URL,
			Timeout:           2 * time.Second,
			RequestCapability: boolPtr(true),
		},
		server.WithToolCapabilities(true),
	)
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	client, err := mcpclient.NewInProcessClient(mcpServer)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("client.Start: %v", err)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = "2025-06-18"
	initReq.Params.ClientInfo.Name = "test-client"
	initReq.Params.ClientInfo.Version = "0.1"
	if _, err := client.Initialize(ctx, initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	callReq := mcp.CallToolRequest{}
	callReq.Params.Name = "request_capability"
	callReq.Params.Arguments = map[string]any{"capability": "send an SMS"}
	result, err := client.CallTool(ctx, callReq)
	if err != nil {
		t.Fatalf("CallTool request_capability: %v", err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("response content = %#v, want one acknowledgment", result.Content)
	}
	text, ok := result.Content[0].(mcp.TextContent)
	if !ok || text.Text != "Capability request acknowledged." {
		t.Fatalf("response content = %#v, want acknowledgment text", result.Content[0])
	}

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	for _, ev := range sink.Events() {
		if ev["kind"] != "tool_call" {
			continue
		}
		meta, _ := ev["metadata"].(map[string]any)
		if meta["tool_name"] == "request_capability" {
			if meta["capability_request"] != true {
				t.Fatalf("request_capability event is missing provenance marker: %v", meta)
			}
			return
		}
	}
	t.Fatalf("request_capability was not recorded as a normal tool_call: events=%v", sink.Events())
}

func TestAddTool_PropagatesIntentToEvent(t *testing.T) {
	sink := newIngestSink(t)
	rec, err := armatureanalytics.NewRecorder(armatureanalytics.Config{
		APIKey:      "test-key",
		EndpointURL: sink.server.URL,
		Timeout:     2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	mcpServer := server.NewMCPServer("test", "1.0",
		server.WithToolCapabilities(true),
		server.WithHooks(rec.Hooks()),
	)
	var sawHandlerArgs map[string]any
	armatureanalytics.InstrumentTool(mcpServer,
		mcp.NewTool("echo",
			mcp.WithDescription("Echoes"),
			mcp.WithString("text", mcp.Description("Text to echo")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			sawHandlerArgs = req.GetArguments()
			return &mcp.CallToolResult{Content: []mcp.Content{
				mcp.TextContent{Type: "text", Text: "echo: " + req.GetArguments()["text"].(string)},
			}}, nil
		},
	)

	client, _ := mcpclient.NewInProcessClient(mcpServer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = client.Start(ctx)
	_, _ = client.Initialize(ctx, mcp.InitializeRequest{})

	callReq := mcp.CallToolRequest{}
	callReq.Params.Name = "echo"
	callReq.Params.Arguments = map[string]any{
		"text": "hi",
		// Legacy spellings on purpose: a cached pre-V1 client must still land
		// events with BOTH key sets after normalization.
		"telemetry": map[string]any{
			"intent":  "verify intent reaches Armature",
			"context": "integration test",
		},
	}
	if _, err := client.CallTool(ctx, callReq); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if err := rec.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if _, leaked := sawHandlerArgs["telemetry"]; leaked {
		t.Errorf("handler saw telemetry in args: %v", sawHandlerArgs)
	}
	if sawHandlerArgs["text"] != "hi" {
		t.Errorf("handler lost real args: %v", sawHandlerArgs)
	}

	var found bool
	for _, ev := range sink.Events() {
		if ev["kind"] != "tool_call" {
			continue
		}
		meta, _ := ev["metadata"].(map[string]any)
		if meta["tool_name"] != "echo" {
			continue
		}
		if meta["intent"] != "verify intent reaches Armature" {
			t.Errorf("metadata.intent = %v, want %q", meta["intent"], "verify intent reaches Armature")
		}
		if meta["user_intent"] != "verify intent reaches Armature" {
			t.Errorf("metadata.user_intent = %v, want the V1 mirror", meta["user_intent"])
		}
		if meta["context"] != "integration test" {
			t.Errorf("metadata.context = %v, want %q", meta["context"], "integration test")
		}
		if meta["agent_thinking"] != "integration test" {
			t.Errorf("metadata.agent_thinking = %v, want the V1 mirror", meta["agent_thinking"])
		}
		// The input_preview should NOT contain the telemetry block.
		if preview, ok := meta["input_preview"].(string); ok {
			if preview != "" && containsString(preview, "telemetry") {
				t.Errorf("input_preview leaked telemetry: %q", preview)
			}
		}
		found = true
	}
	if !found {
		t.Fatalf("did not see tool_call event for echo; sink=%v", sink.Events())
	}
}

func containsString(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestInstrumentTool_PreexistingTelemetryInput_ReachesHandler(t *testing.T) {
	sink := newIngestSink(t)
	rec, err := armatureanalytics.NewRecorder(armatureanalytics.Config{
		APIKey:      "test-key",
		EndpointURL: sink.server.URL,
		Timeout:     2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	mcpServer := server.NewMCPServer("test", "1.0",
		server.WithToolCapabilities(true),
		server.WithHooks(rec.Hooks()),
	)
	var sawArgs map[string]any
	armatureanalytics.InstrumentTool(mcpServer,
		mcp.NewTool("legacy",
			mcp.WithDescription("Tool with its own telemetry input"),
			mcp.WithString("telemetry", mcp.Required(), mcp.Description("A real input, not the analytics block")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			sawArgs = req.GetArguments()
			return &mcp.CallToolResult{Content: []mcp.Content{
				mcp.TextContent{Type: "text", Text: "ok"},
			}}, nil
		},
	)

	client, err := mcpclient.NewInProcessClient(mcpServer)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("client.Start: %v", err)
	}
	if _, err := client.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	callReq := mcp.CallToolRequest{}
	callReq.Params.Name = "legacy"
	callReq.Params.Arguments = map[string]any{"telemetry": "device-42"}
	if _, err := client.CallTool(ctx, callReq); err != nil {
		t.Fatalf("CallTool legacy: %v", err)
	}
	if err := rec.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if sawArgs["telemetry"] != "device-42" {
		t.Fatalf("handler lost its real telemetry input: %v", sawArgs)
	}
}

func TestRecorder_ClosePreventsFurtherEmission(t *testing.T) {
	sink := newIngestSink(t)
	rec, _ := armatureanalytics.NewRecorder(armatureanalytics.Config{
		APIKey:      "test-key",
		EndpointURL: sink.server.URL,
	})
	_ = rec.Close(context.Background())

	mcpServer := server.NewMCPServer("s", "1.0", server.WithToolCapabilities(true), server.WithHooks(rec.Hooks()))
	mcpServer.AddTool(
		mcp.NewTool("ping"),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "pong"}}}, nil
		},
	)
	client, _ := mcpclient.NewInProcessClient(mcpServer)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = client.Start(ctx)
	_, _ = client.Initialize(ctx, mcp.InitializeRequest{})
	req := mcp.CallToolRequest{}
	req.Params.Name = "ping"
	_, _ = client.CallTool(ctx, req)

	if got := sink.Events(); len(got) != 0 {
		t.Fatalf("closed recorder emitted %d events; want 0", len(got))
	}
	if rec.Dropped() == 0 {
		t.Errorf("expected at least one dropped event after Close")
	}
}
