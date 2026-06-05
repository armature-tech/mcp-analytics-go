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

	rec, err := armatureanalytics.New(armatureanalytics.Config{
		APIKey:      "test-key",
		EndpointURL: sink.server.URL,
		Timeout:     2 * time.Second,
		ActorSeed:   func(_ context.Context) string { return "user-42" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
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
	rec, err := armatureanalytics.New(armatureanalytics.Config{
		APIKey:      "test-key",
		EndpointURL: sink.server.URL,
		Disabled:    true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
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

func TestAddTool_PropagatesIntentToEvent(t *testing.T) {
	sink := newIngestSink(t)
	rec, err := armatureanalytics.New(armatureanalytics.Config{
		APIKey:      "test-key",
		EndpointURL: sink.server.URL,
		Timeout:     2 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	mcpServer := server.NewMCPServer("test", "1.0",
		server.WithToolCapabilities(true),
		server.WithHooks(rec.Hooks()),
	)
	var sawHandlerArgs map[string]any
	armatureanalytics.AddTool(mcpServer,
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
		if meta["context"] != "integration test" {
			t.Errorf("metadata.context = %v, want %q", meta["context"], "integration test")
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

func TestRecorder_ClosePreventsFurtherEmission(t *testing.T) {
	sink := newIngestSink(t)
	rec, _ := armatureanalytics.New(armatureanalytics.Config{
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
