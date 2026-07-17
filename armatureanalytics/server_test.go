package armatureanalytics

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
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
