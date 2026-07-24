package armatureanalytics

import (
	"context"
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

func TestNewMCPServerWithConfig_RequestCapabilityDefaultsOn(t *testing.T) {
	// Explicit opt-out removes the tool even with a delivery path.
	off, shutdownOff := NewMCPServerWithConfig("test", "0", Config{
		RequestCapability: boolPtr(false),
		Emit:              func(context.Context, Batch) error { return nil },
	})
	if tool := off.GetTool(requestCapabilityToolName); tool != nil {
		t.Fatal("request_capability should be absent when explicitly disabled")
	}
	if err := shutdownOff(context.Background()); err != nil {
		t.Fatalf("shutdown opted-out server: %v", err)
	}

	// On by default once a delivery path is configured, with no opt-in.
	on, shutdownOn := NewMCPServerWithConfig("test", "0", Config{
		Emit: func(context.Context, Batch) error { return nil },
	})
	registered := on.GetTool(requestCapabilityToolName)
	if registered == nil {
		t.Fatal("request_capability should be on by default with a delivery path")
	}
	if got := registered.Tool.Description; got != requestCapabilityToolDescription {
		t.Fatalf("description = %q, want %q", got, requestCapabilityToolDescription)
	}
	if _, exists := registered.Tool.InputSchema.Properties["telemetry"]; exists {
		t.Fatal("request_capability should not advertise telemetry")
	}
	if got := registered.Tool.InputSchema.Required; len(got) != 1 || got[0] != "capability" {
		t.Fatalf("required = %v, want [capability]", got)
	}
	capability, _ := registered.Tool.InputSchema.Properties["capability"].(map[string]any)
	if got := capability["description"]; got != requestCapabilityArgDescription {
		t.Fatalf("capability description = %q, want %q", got, requestCapabilityArgDescription)
	}
	if err := shutdownOn(context.Background()); err != nil {
		t.Fatalf("shutdown enabled server: %v", err)
	}
}

func TestNewMCPServerWithConfig_RequestCapabilityRequiresDelivery(t *testing.T) {
	s, shutdown := NewMCPServerWithConfig("test", "0", Config{RequestCapability: boolPtr(true)})
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	if tool := s.GetTool(requestCapabilityToolName); tool != nil {
		t.Fatal("request_capability should not be registered without a delivery path")
	}
}

func TestAddRequestCapabilityToolRejectsNameCollision(t *testing.T) {
	rec, err := NewRecorder(Config{Emit: func(context.Context, Batch) error { return nil }})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	s := mcpserver.NewMCPServer("test", "0")
	s.AddTool(mcp.NewTool(requestCapabilityToolName), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("customer tool"), nil
	})
	if err := AddRequestCapabilityTool(s, rec); err == nil {
		t.Fatal("expected reserved-name collision error")
	}
}

func TestAddRequestCapabilityToolRequiresRecorder(t *testing.T) {
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
			if err := AddRequestCapabilityTool(s, rec); err == nil {
				t.Fatal("expected inactive-recorder error")
			}
			if tool := s.GetTool(requestCapabilityToolName); tool != nil {
				t.Fatal("request_capability should not be registered without an active recorder")
			}
		})
	}
}

func TestRequestCapabilityRejectsCallsAfterRecorderClose(t *testing.T) {
	rec, err := NewRecorder(Config{Emit: func(context.Context, Batch) error { return nil }})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	s := mcpserver.NewMCPServer("test", "0")
	if err := AddRequestCapabilityTool(s, rec); err != nil {
		t.Fatalf("AddRequestCapabilityTool: %v", err)
	}
	if err := rec.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = requestCapabilityToolName
	req.Params.Arguments = map[string]any{"capability": "send an SMS"}
	result, err := s.GetTool(requestCapabilityToolName).Handler(context.Background(), req)
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

func TestRequestCapabilityReservationCompletesDuringConcurrentClose(t *testing.T) {
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
	if err := AddRequestCapabilityTool(s, rec); err != nil {
		t.Fatalf("AddRequestCapabilityTool: %v", err)
	}
	req := &mcp.CallToolRequest{}
	req.Params.Name = requestCapabilityToolName
	req.Params.Arguments = map[string]any{"capability": "send an SMS"}
	rec.onBeforeAny(context.Background(), int64(1), mcp.MethodToolsCall, req)
	result, err := s.GetTool(requestCapabilityToolName).Handler(context.Background(), *req)
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

func TestRequestCapabilityProvenanceFollowsSDKHandler(t *testing.T) {
	var batches []Batch
	rec, err := NewRecorder(Config{
		RequestCapability: boolPtr(true),
		Delivery:          DeliveryAwait,
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
	if err := AddRequestCapabilityTool(s, rec); err != nil {
		t.Fatalf("AddRequestCapabilityTool: %v", err)
	}
	req := &mcp.CallToolRequest{}
	req.Params.Name = requestCapabilityToolName
	req.Params.Arguments = map[string]any{"capability": "send an SMS"}

	rec.onBeforeAny(context.Background(), int64(1), mcp.MethodToolsCall, req)
	result, err := s.GetTool(requestCapabilityToolName).Handler(context.Background(), *req)
	if err != nil {
		t.Fatalf("SDK handler: %v", err)
	}
	rec.onSuccess(context.Background(), int64(1), mcp.MethodToolsCall, req, result)
	if result.Meta != nil {
		t.Fatalf("SDK provenance marker leaked into result metadata: %#v", result.Meta)
	}

	// mcp-go registrations are last-write-wins. Replacing the injected handler
	// must also remove SDK provenance even though the tool name is unchanged.
	s.AddTool(mcp.NewTool(requestCapabilityToolName), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("customer tool"), nil
	})
	rec.onBeforeAny(context.Background(), int64(2), mcp.MethodToolsCall, req)
	result, err = s.GetTool(requestCapabilityToolName).Handler(context.Background(), *req)
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
	if toolEvents[0].Metadata["capability_request"] != true {
		t.Fatalf("SDK event missing provenance: %#v", toolEvents[0].Metadata)
	}
	if _, marked := toolEvents[1].Metadata["capability_request"]; marked {
		t.Fatalf("customer event inherited SDK provenance: %#v", toolEvents[1].Metadata)
	}
}

func TestNewMCPServerWithConfig_DisabledSuppressesRequestCapability(t *testing.T) {
	s, shutdown := NewMCPServerWithConfig("test", "0", Config{
		Disabled:          true,
		RequestCapability: boolPtr(true),
	})
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	if tool := s.GetTool(requestCapabilityToolName); tool != nil {
		t.Fatal("request_capability should not be registered when the SDK is disabled")
	}
}
