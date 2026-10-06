package armatureanalytics

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func TestNewMCPServer_NoAPIKey_NoOp(t *testing.T) {
	t.Setenv("ANALYTICS_INGEST_API_KEY", "")
	t.Setenv("ANALYTICS_INGEST_URL", "")

	s, shutdown := NewMCPServer("test", "0")
	if s == nil {
		t.Fatal("server should be non-nil even when analytics disabled")
	}
	if shutdown == nil {
		t.Fatal("shutdown should be non-nil even when analytics disabled")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown should be a no-op when disabled, got: %v", err)
	}
}

func TestNewMCPServerWithConfig_BuildsRecorderWhenAPIKeySet(t *testing.T) {
	s, shutdown := NewMCPServerWithConfig("test", "0", Config{
		APIKey:      "test-key",
		EndpointURL: "https://example.invalid/ingest",
	})
	if s == nil {
		t.Fatal("server nil")
	}
	if shutdown == nil {
		t.Fatal("shutdown nil")
	}
	// Register a tool to confirm the returned server is usable.
	InstrumentTool(s, mcp.NewTool("noop", mcp.WithDescription("noop")), func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown returned error: %v", err)
	}
}

func TestNewMCPServerWithConfig_InstrumentToolHonorsCaptureOff(t *testing.T) {
	s, shutdown := NewMCPServerWithConfig("test", "0", Config{CaptureTelemetry: boolPtr(false)})
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	tool := mcp.NewTool("search", mcp.WithDescription("Search"), mcp.WithString("q"))
	InstrumentTool(s, tool, func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})

	listed := listTool(t, s, "search")
	if _, exists := listed.InputSchema.Properties["telemetry"]; exists {
		t.Fatal("plain InstrumentTool injected telemetry despite the server capture policy")
	}
	if listed.Description != tool.Description {
		t.Fatalf("description changed with capture off: %q", listed.Description)
	}
}

func TestNewMCPServerWithConfig_NegativeTimeout_Normalized(t *testing.T) {
	var captured error
	s, shutdown := NewMCPServerWithConfig("test", "0", Config{
		APIKey:  "test-key",
		Timeout: -1, // NewClient normalizes non-positive timeouts to DefaultTimeout
		OnError: func(err error, _ Batch) { captured = err },
	})
	if s == nil {
		t.Fatal("server should be returned")
	}
	if captured != nil {
		t.Fatalf("OnError should not fire for a normalized timeout, got: %v", captured)
	}
	if shutdown == nil {
		t.Fatal("shutdown nil")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown returned error: %v", err)
	}
}

func TestNewMCPServerWithConfig_SendFeedbackDefaultsOn(t *testing.T) {
	emit := func(context.Context, Batch) error { return nil }
	// Disabled by the new key, by the deprecated alias, and when the new key
	// (false) wins over the alias (true).
	for name, cfg := range map[string]Config{
		"SendFeedback false":                 {SendFeedback: boolPtr(false)},
		"RequestCapability false":            {RequestCapability: boolPtr(false)},
		"SendFeedback false wins over alias": {SendFeedback: boolPtr(false), RequestCapability: boolPtr(true)},
	} {
		cfg.Emit = emit
		off, shutdownOff := NewMCPServerWithConfig("test", "0", cfg)
		if off.GetTool(SendFeedbackToolName) != nil || off.GetTool("request_capability") != nil || SendFeedbackRegistered(off) {
			t.Fatalf("%s: send_feedback should be absent", name)
		}
		if err := shutdownOff(context.Background()); err != nil {
			t.Fatalf("%s: shutdown: %v", name, err)
		}
	}

	// On by default once a delivery path is configured, and when the new key
	// (true) wins over the alias (false).
	for name, cfg := range map[string]Config{
		"default":                           {},
		"SendFeedback true wins over alias": {SendFeedback: boolPtr(true), RequestCapability: boolPtr(false)},
		"RequestCapability true":            {RequestCapability: boolPtr(true)},
	} {
		cfg.Emit = emit
		on, shutdownOn := NewMCPServerWithConfig("test", "0", cfg)
		registered := on.GetTool(SendFeedbackToolName)
		if registered == nil {
			t.Fatalf("%s: send_feedback should be on", name)
		}
		if on.GetTool("request_capability") != nil {
			t.Fatalf("%s: no tool may be registered under the old name", name)
		}
		if registered.Tool.Name != "send_feedback" {
			t.Fatalf("%s: name = %q", name, registered.Tool.Name)
		}
		if got := registered.Tool.Description; got != "Records that the user asked for something these tools cannot do, so the developers of this server can add it. It changes no data and contacts no one. Call it whenever you cannot do what the user asked with these tools, including when you send them to an app, a website or a manual step instead. Then answer them as usual." {
			t.Fatalf("%s: description = %q", name, got)
		}
		// ChatGPT's app directory requires the three hints as explicit
		// booleans, and mcp-go's defaults would mark the tool destructive and
		// open-world.
		if got := canonicalJSON(t, registered.Tool.Annotations); got != `{"destructiveHint":false,"idempotentHint":false,"openWorldHint":false,"readOnlyHint":false,"title":"Send feedback"}` {
			t.Fatalf("%s: annotations = %s", name, got)
		}
		if _, exists := registered.Tool.InputSchema.Properties["telemetry"]; exists {
			t.Fatalf("%s: send_feedback should not advertise telemetry", name)
		}
		if got := registered.Tool.InputSchema.Required; len(got) != 1 || got[0] != "capability" {
			t.Fatalf("%s: required = %v, want [capability]", name, got)
		}
		capability, _ := registered.Tool.InputSchema.Properties["capability"].(map[string]any)
		if got := capability["description"]; got != "One English sentence describing the missing capability needed for the user's task. Translate the summary into English even when the user writes in another language. Describe generic actions and roles. Omit names, contacts, IDs, credentials and all tool argument values." {
			t.Fatalf("%s: capability description = %q", name, got)
		}
		if capability["minLength"] != 1 || capability["maxLength"] != 1000 {
			t.Fatalf("%s: capability bounds = %v", name, capability)
		}
		if err := shutdownOn(context.Background()); err != nil {
			t.Fatalf("%s: shutdown: %v", name, err)
		}
	}
}

// On by default, the SDK yields to a customer tool named send_feedback and
// reports nothing; explicitly enabled, the collision is an OnError error that
// names send_feedback and the SendFeedback key.
func TestSendFeedbackInjectionYieldsByDefaultAndErrorsWhenExplicit(t *testing.T) {
	rec, err := NewRecorder(Config{Emit: func(context.Context, Batch) error { return nil }})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close(context.Background()) })
	for name, tc := range map[string]struct {
		setting *bool
		wantErr bool
	}{
		"default":           {nil, false},
		"explicit":          {boolPtr(true), true},
		"explicit by alias": {nil, true},
	} {
		s := mcpserver.NewMCPServer("test", "0")
		s.AddTool(mcp.NewTool(SendFeedbackToolName, mcp.WithDescription("Customer feedback form")), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("customer tool"), nil
		})
		var reported []error
		cfg := Config{SendFeedback: tc.setting, OnError: func(err error, _ Batch) { reported = append(reported, err) }}
		if name == "explicit by alias" {
			cfg.RequestCapability = boolPtr(true)
		}
		injectSendFeedback(s, rec, cfg)
		if got := s.GetTool(SendFeedbackToolName).Tool.Description; got != "Customer feedback form" {
			t.Fatalf("%s: customer tool replaced: %q", name, got)
		}
		if SendFeedbackRegistered(s) {
			t.Fatalf("%s: registry claims the SDK tool", name)
		}
		if !tc.wantErr {
			if len(reported) != 0 {
				t.Fatalf("%s: default-on collision reported: %v", name, reported)
			}
			continue
		}
		if len(reported) != 1 || !strings.Contains(reported[0].Error(), `"send_feedback"`) || !strings.Contains(reported[0].Error(), "SendFeedback") {
			t.Fatalf("%s: reported = %v, want one error naming send_feedback and SendFeedback", name, reported)
		}
	}
}

func TestNewMCPServerWithConfig_SendFeedbackRequiresDelivery(t *testing.T) {
	s, shutdown := NewMCPServerWithConfig("test", "0", Config{SendFeedback: boolPtr(true)})
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	if tool := s.GetTool(SendFeedbackToolName); tool != nil {
		t.Fatal("send_feedback should not be registered without a delivery path")
	}
}

func TestAddSendFeedbackToolRejectsNameCollision(t *testing.T) {
	rec, err := NewRecorder(Config{Emit: func(context.Context, Batch) error { return nil }})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	s := mcpserver.NewMCPServer("test", "0")
	s.AddTool(mcp.NewTool(SendFeedbackToolName), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("customer tool"), nil
	})
	if err := AddSendFeedbackTool(s, rec); err == nil {
		t.Fatal("expected reserved-name collision error")
	}
}

func TestAddSendFeedbackToolRequiresRecorder(t *testing.T) {
	disabled, err := NewRecorder(Config{Disabled: true})
	if err != nil {
		t.Fatalf("NewRecorder disabled: %v", err)
	}
	closed, err := NewRecorder(Config{Emit: func(context.Context, Batch) error { return nil }})
	if err != nil {
		t.Fatalf("NewRecorder active: %v", err)
	}
	if err := closed.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for name, rec := range map[string]*Recorder{
		"nil":      nil,
		"disabled": disabled,
		"closed":   closed,
	} {
		t.Run(name, func(t *testing.T) {
			s := mcpserver.NewMCPServer("test", "0")
			if err := AddSendFeedbackTool(s, rec); err == nil {
				t.Fatal("expected inactive-recorder error")
			}
			if tool := s.GetTool(SendFeedbackToolName); tool != nil {
				t.Fatal("send_feedback should not be registered without an active recorder")
			}
		})
	}
}

func TestSendFeedbackRejectsCallsAfterRecorderClose(t *testing.T) {
	rec, err := NewRecorder(Config{Emit: func(context.Context, Batch) error { return nil }})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	s := mcpserver.NewMCPServer("test", "0")
	if err := AddSendFeedbackTool(s, rec); err != nil {
		t.Fatalf("AddSendFeedbackTool: %v", err)
	}
	if err := rec.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = SendFeedbackToolName
	req.Params.Arguments = map[string]any{"capability": "send an SMS"}
	result, err := s.GetTool(SendFeedbackToolName).Handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !result.IsError {
		t.Fatalf("result IsError = false, want unavailable error: %#v", result)
	}
	if result.Meta != nil {
		t.Fatalf("inactive call should not carry capability provenance: %#v", result.Meta)
	}
}

func TestSendFeedbackReservationCompletesDuringConcurrentClose(t *testing.T) {
	var batches []Batch
	rec, err := NewRecorder(Config{
		Delivery: DeliveryAwait,
		Emit: func(_ context.Context, batch Batch) error {
			batches = append(batches, batch)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	s := mcpserver.NewMCPServer("test", "0")
	if err := AddSendFeedbackTool(s, rec); err != nil {
		t.Fatalf("AddSendFeedbackTool: %v", err)
	}
	req := &mcp.CallToolRequest{}
	req.Params.Name = SendFeedbackToolName
	req.Params.Arguments = map[string]any{"capability": "send an SMS"}
	rec.onBeforeAny(context.Background(), int64(1), mcp.MethodToolsCall, req)
	result, err := s.GetTool(SendFeedbackToolName).Handler(context.Background(), *req)
	if err != nil || result.IsError {
		t.Fatalf("handler result = %#v, err = %v", result, err)
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- rec.Close(context.Background()) }()
	deadline := time.Now().Add(time.Second)
	for !rec.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !rec.closed.Load() {
		t.Fatal("Close did not begin")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before the reserved event completed: %v", err)
	default:
	}

	rec.onSuccess(context.Background(), int64(1), mcp.MethodToolsCall, req, result)
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if result.Meta != nil {
		t.Fatalf("reservation marker leaked into result metadata: %#v", result.Meta)
	}
	if len(batches) != 1 || len(batches[0].Events) == 0 {
		t.Fatalf("reserved capability event was not delivered: %#v", batches)
	}
	event := batches[0].Events[len(batches[0].Events)-1]
	if event.Metadata["capability_request"] != true {
		t.Fatalf("reserved event missing capability provenance: %#v", event.Metadata)
	}
}

func TestSendFeedbackProvenanceFollowsSDKHandler(t *testing.T) {
	var batches []Batch
	rec, err := NewRecorder(Config{
		SendFeedback: boolPtr(true),
		Delivery:     DeliveryAwait,
		Emit: func(_ context.Context, batch Batch) error {
			batches = append(batches, batch)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	s := mcpserver.NewMCPServer("test", "0")
	if err := AddSendFeedbackTool(s, rec); err != nil {
		t.Fatalf("AddSendFeedbackTool: %v", err)
	}
	req := &mcp.CallToolRequest{}
	req.Params.Name = SendFeedbackToolName
	req.Params.Arguments = map[string]any{"capability": "send an SMS"}

	rec.onBeforeAny(context.Background(), int64(1), mcp.MethodToolsCall, req)
	result, err := s.GetTool(SendFeedbackToolName).Handler(context.Background(), *req)
	if err != nil {
		t.Fatalf("SDK handler: %v", err)
	}
	rec.onSuccess(context.Background(), int64(1), mcp.MethodToolsCall, req, result)
	if result.Meta != nil {
		t.Fatalf("SDK provenance marker leaked into result metadata: %#v", result.Meta)
	}

	// mcp-go registrations are last-write-wins. Replacing the injected handler
	// must also remove SDK provenance even though the tool name is unchanged.
	s.AddTool(mcp.NewTool(SendFeedbackToolName), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("customer tool"), nil
	})
	rec.onBeforeAny(context.Background(), int64(2), mcp.MethodToolsCall, req)
	result, err = s.GetTool(SendFeedbackToolName).Handler(context.Background(), *req)
	if err != nil {
		t.Fatalf("customer handler: %v", err)
	}
	rec.onSuccess(context.Background(), int64(2), mcp.MethodToolsCall, req, result)

	var toolEvents []Event
	for _, batch := range batches {
		for _, event := range batch.Events {
			if event.Kind == KindToolCall {
				toolEvents = append(toolEvents, event)
			}
		}
	}
	if len(toolEvents) != 2 {
		t.Fatalf("tool events = %d, want 2: %#v", len(toolEvents), toolEvents)
	}
	if toolEvents[0].Metadata["capability_request"] != true || toolEvents[0].Metadata["tool_name"] != "send_feedback" {
		t.Fatalf("SDK event missing provenance: %#v", toolEvents[0].Metadata)
	}
	if _, marked := toolEvents[1].Metadata["capability_request"]; marked {
		t.Fatalf("customer event inherited SDK provenance: %#v", toolEvents[1].Metadata)
	}
}

func TestNewMCPServerWithConfig_DisabledSuppressesSendFeedback(t *testing.T) {
	s, shutdown := NewMCPServerWithConfig("test", "0", Config{
		Disabled:     true,
		SendFeedback: boolPtr(true),
	})
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	if tool := s.GetTool(SendFeedbackToolName); tool != nil {
		t.Fatal("send_feedback should not be registered when the SDK is disabled")
	}
}

// canonicalJSON marshals v, then re-marshals it through a map so keys sort.
func canonicalJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
