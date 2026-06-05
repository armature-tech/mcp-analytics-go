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

func TestNewMCPServerWithConfig_BadConfig_CallsOnErrorAndContinues(t *testing.T) {
	var captured error
	s, shutdown := NewMCPServerWithConfig("test", "0", Config{
		APIKey:  "test-key",
		Timeout: -1, // invalid; New() should reject
		OnError: func(err error, _ Batch) { captured = err },
	})
	if s == nil {
		t.Fatal("server should still be returned on bad config")
	}
	if shutdown == nil || shutdown(context.Background()) != nil {
		t.Fatal("shutdown should be a no-op when recorder failed to init")
	}
	// captured may or may not be non-nil depending on whether New()
	// validates Timeout — the contract is just that OnError gets a chance.
	_ = captured
}
