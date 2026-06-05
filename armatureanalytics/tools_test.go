package armatureanalytics

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestExtractTelemetryFromArgs_AllFields(t *testing.T) {
	args := map[string]any{
		"text": "hi",
		"telemetry": map[string]any{
			"intent":            "summarise the latest cpu metric",
			"context":           "user is on the cpu-spike dashboard",
			"frustration_level": "medium",
		},
	}
	tel, cleaned := extractTelemetryFromArgs(args)
	if tel.Intent != "summarise the latest cpu metric" {
		t.Errorf("Intent = %q", tel.Intent)
	}
	if tel.Context != "user is on the cpu-spike dashboard" {
		t.Errorf("Context = %q", tel.Context)
	}
	if tel.FrustrationLevel != "medium" {
		t.Errorf("FrustrationLevel = %q", tel.FrustrationLevel)
	}
	if _, ok := cleaned["telemetry"]; ok {
		t.Errorf("cleaned args still contain telemetry")
	}
	if cleaned["text"] != "hi" {
		t.Errorf("text arg dropped: %v", cleaned)
	}
}

func TestExtractTelemetryFromArgs_StringEncoded(t *testing.T) {
	args := map[string]any{
		"telemetry": `{"intent":"x"}`,
	}
	tel, cleaned := extractTelemetryFromArgs(args)
	if tel.Intent != "x" {
		t.Errorf("Intent = %q", tel.Intent)
	}
	if _, ok := cleaned["telemetry"]; ok {
		t.Errorf("cleaned args still contain telemetry")
	}
}

func TestExtractTelemetryFromArgs_Missing(t *testing.T) {
	args := map[string]any{"text": "hi"}
	tel, cleaned := extractTelemetryFromArgs(args)
	if tel != (Telemetry{}) {
		t.Errorf("Telemetry = %+v, want zero", tel)
	}
	if &cleaned == &args {
		// Pointer comparison only meaningful if caller mutates; safety net.
	}
	if cleaned["text"] != "hi" {
		t.Errorf("text arg dropped")
	}
}

func TestDecorateInputSchemaWithTelemetry_AddsOptionalTelemetry(t *testing.T) {
	tool := mcp.NewTool("echo",
		mcp.WithDescription("Echoes"),
		mcp.WithString("text", mcp.Required(), mcp.Description("Text to echo")),
	)
	decorated := DecorateInputSchemaWithTelemetry(tool)

	if decorated.InputSchema.Properties["telemetry"] == nil {
		t.Fatalf("telemetry property not injected")
	}
	for _, r := range decorated.InputSchema.Required {
		if r == "telemetry" {
			t.Errorf("telemetry must NOT be in required (expected optional intent)")
		}
	}
	tel, _ := decorated.InputSchema.Properties["telemetry"].(map[string]any)
	if tel == nil || tel["type"] != "object" {
		t.Errorf("telemetry property shape wrong: %+v", tel)
	}
	props, _ := tel["properties"].(map[string]any)
	if _, ok := props["intent"]; !ok {
		t.Errorf("missing intent sub-property")
	}
	if _, ok := props["frustration_level"]; !ok {
		t.Errorf("missing frustration_level sub-property")
	}
}

func TestDecorateInputSchemaWithTelemetry_RawSchemaPath(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)
	tool := mcp.Tool{Name: "echo", RawInputSchema: raw}

	decorated := DecorateInputSchemaWithTelemetry(tool)
	if len(decorated.RawInputSchema) == 0 {
		t.Fatalf("RawInputSchema dropped")
	}
	var parsed map[string]any
	if err := json.Unmarshal(decorated.RawInputSchema, &parsed); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	props, _ := parsed["properties"].(map[string]any)
	if _, ok := props["telemetry"]; !ok {
		t.Errorf("telemetry not injected into raw schema; got %v", parsed)
	}
	req, _ := parsed["required"].([]any)
	for _, r := range req {
		if r == "telemetry" {
			t.Errorf("telemetry must NOT be in raw schema required")
		}
	}
}

func TestWrapHandler_StripsTelemetryAndPropagatesViaContext(t *testing.T) {
	var (
		sawTel  Telemetry
		sawArgs map[string]any
	)
	inner := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sawTel = TelemetryFromContext(ctx)
		sawArgs = req.GetArguments()
		return &mcp.CallToolResult{}, nil
	}
	wrapped := WrapHandler(inner)

	req := mcp.CallToolRequest{}
	req.Params.Name = "echo"
	req.Params.Arguments = map[string]any{
		"text": "hi",
		"telemetry": map[string]any{
			"intent": "test the wrap path",
		},
	}
	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped: %v", err)
	}
	if sawTel.Intent != "test the wrap path" {
		t.Errorf("inner handler didn't see intent on ctx: %+v", sawTel)
	}
	if _, ok := sawArgs["telemetry"]; ok {
		t.Errorf("inner handler still saw telemetry in args: %v", sawArgs)
	}
	if sawArgs["text"] != "hi" {
		t.Errorf("inner handler lost real args: %v", sawArgs)
	}
}
