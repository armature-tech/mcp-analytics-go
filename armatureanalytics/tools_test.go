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
			"user_turn":        float64(2),
			"user_intent":      "summarise the latest cpu metric",
			"agent_thinking":   "user is on the cpu-spike dashboard",
			"user_frustration": "medium",
		},
	}
	tel, cleaned := extractTelemetryFromArgs(args)
	if tel.UserTurn != 2 {
		t.Errorf("UserTurn = %d", tel.UserTurn)
	}
	if tel.UserIntent != "summarise the latest cpu metric" {
		t.Errorf("UserIntent = %q", tel.UserIntent)
	}
	if tel.AgentThinking != "user is on the cpu-spike dashboard" {
		t.Errorf("AgentThinking = %q", tel.AgentThinking)
	}
	if tel.UserFrustration != "medium" {
		t.Errorf("UserFrustration = %q", tel.UserFrustration)
	}
	// Legacy mirrors are filled so a not-yet-updated ingest keeps reading.
	if tel.Intent != tel.UserIntent || tel.Context != tel.AgentThinking || tel.FrustrationLevel != tel.UserFrustration {
		t.Errorf("legacy mirrors not filled: %+v", tel)
	}
	if _, ok := cleaned["telemetry"]; ok {
		t.Errorf("cleaned args still contain telemetry")
	}
	if cleaned["text"] != "hi" {
		t.Errorf("text arg dropped: %v", cleaned)
	}
}

func TestExtractTelemetryFromArgs_LegacyKeysNormalize(t *testing.T) {
	// A client holding a cached pre-V1 tool schema sends the old spellings;
	// they normalize onto the V1 fields.
	args := map[string]any{
		"telemetry": map[string]any{
			"intent":            "summarise the latest cpu metric",
			"context":           "user is on the cpu-spike dashboard",
			"frustration_level": "high",
		},
	}
	tel, _ := extractTelemetryFromArgs(args)
	if tel.UserIntent != "summarise the latest cpu metric" {
		t.Errorf("UserIntent = %q", tel.UserIntent)
	}
	if tel.AgentThinking != "user is on the cpu-spike dashboard" {
		t.Errorf("AgentThinking = %q", tel.AgentThinking)
	}
	if tel.UserFrustration != "high" {
		t.Errorf("UserFrustration = %q", tel.UserFrustration)
	}
}

func TestExtractTelemetryFromArgs_V1WinsOverLegacy(t *testing.T) {
	args := map[string]any{
		"telemetry": map[string]any{
			"user_intent": "v1 wording",
			"intent":      "legacy wording",
		},
	}
	tel, _ := extractTelemetryFromArgs(args)
	if tel.UserIntent != "v1 wording" || tel.Intent != "v1 wording" {
		t.Errorf("V1 must win both spellings: %+v", tel)
	}
}

func TestExtractTelemetryFromArgs_UserTurnGuards(t *testing.T) {
	// Fractional, zero, and negative turns are dropped rather than coerced;
	// integral floats are accepted; off-spec frustration values are dropped.
	for _, bad := range []any{1.9, float64(0), float64(-1)} {
		args := map[string]any{"telemetry": map[string]any{"user_turn": bad, "user_frustration": "annoyed"}}
		tel, _ := extractTelemetryFromArgs(args)
		if tel.UserTurn != 0 {
			t.Errorf("user_turn %v should be dropped, got %d", bad, tel.UserTurn)
		}
		if tel.UserFrustration != "" {
			t.Errorf("off-spec frustration should be dropped, got %q", tel.UserFrustration)
		}
	}
	tel, _ := extractTelemetryFromArgs(map[string]any{"telemetry": map[string]any{"user_turn": 2.0}})
	if tel.UserTurn != 2 {
		t.Errorf("integral float user_turn should be kept, got %d", tel.UserTurn)
	}
}

func TestExtractTelemetryFromArgs_PresentV1KeyShadowsLegacy(t *testing.T) {
	// An explicitly blank V1 key suppresses the legacy value instead of
	// falling back to it — same first-string-wins rule as TS/Python. The
	// result is an empty block (readers treat blank and absent identically).
	args := map[string]any{
		"telemetry": map[string]any{"user_intent": "", "intent": "stale legacy wording"},
	}
	tel, _ := extractTelemetryFromArgs(args)
	if tel.UserIntent != "" || tel.Intent != "" {
		t.Errorf("blank V1 key must shadow legacy: %+v", tel)
	}
}

func TestAppendTelemetryHint_Idempotent(t *testing.T) {
	once := AppendTelemetryHint("Echoes.")
	if once == "Echoes." {
		t.Fatalf("hint not appended")
	}
	if AppendTelemetryHint(once) != once {
		t.Errorf("hint appended twice")
	}
	// A description written by a pre-V1 SDK build keeps its old hint without
	// gaining a second one.
	legacy := "Echoes." + telemetryDescriptionHintLegacy
	if AppendTelemetryHint(legacy) != legacy {
		t.Errorf("legacy-hinted description modified")
	}
	if AppendTelemetryHint("") == "" {
		t.Errorf("empty description should become the hint")
	}
}

func TestExtractTelemetryFromArgs_StringEncoded(t *testing.T) {
	args := map[string]any{
		"text":      "hi",
		"telemetry": `{"intent":"x"}`,
	}
	tel, cleaned := extractTelemetryFromArgs(args)
	if tel.UserIntent != "x" || tel.Intent != "x" {
		t.Errorf("string-encoded legacy intent not normalized: %+v", tel)
	}
	if _, ok := cleaned["telemetry"]; ok {
		t.Errorf("cleaned args still contain telemetry")
	}
	if cleaned["text"] != "hi" {
		t.Errorf("sibling arg dropped when telemetry was string-encoded: %v", cleaned)
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
	decorated, ok := DecorateInputSchemaWithTelemetry(tool)
	if !ok {
		t.Fatalf("expected ok=true for a plain schema")
	}

	if decorated.InputSchema.Properties["telemetry"] == nil {
		t.Fatalf("telemetry property not injected")
	}
	if _, leaked := tool.InputSchema.Properties["telemetry"]; leaked {
		t.Fatalf("original tool's Properties map was mutated")
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
	for _, key := range []string{"user_turn", "user_intent", "agent_thinking", "user_frustration"} {
		if _, ok := props[key]; !ok {
			t.Errorf("missing %s sub-property", key)
		}
	}
}

func TestDecorateInputSchemaWithTelemetry_RawSchemaPath(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)
	tool := mcp.Tool{Name: "echo", RawInputSchema: raw}

	decorated, ok := DecorateInputSchemaWithTelemetry(tool)
	if !ok {
		t.Fatalf("expected ok=true for a plain raw schema")
	}
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

func TestDecorateInputSchemaWithTelemetry_PreexistingPropertyUntouched(t *testing.T) {
	tool := mcp.NewTool("legacy",
		mcp.WithDescription("Has its own telemetry input"),
		mcp.WithString("telemetry", mcp.Required(), mcp.Description("A real input, not ours")),
	)
	decorated, ok := DecorateInputSchemaWithTelemetry(tool)
	if ok {
		t.Fatalf("expected ok=false for a pre-existing telemetry property")
	}
	got, _ := decorated.InputSchema.Properties["telemetry"].(map[string]any)
	if got == nil || got["type"] != "string" {
		t.Fatalf("pre-existing telemetry property was overwritten: %+v", got)
	}
}

func TestDecorateInputSchemaWithTelemetry_RawSchemaPreexistingPropertyUntouched(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"telemetry":{"type":"string"}},"required":["telemetry"]}`)
	tool := mcp.Tool{Name: "legacy", RawInputSchema: raw}
	decorated, ok := DecorateInputSchemaWithTelemetry(tool)
	if ok {
		t.Fatalf("expected ok=false for a pre-existing raw telemetry property")
	}
	if string(decorated.RawInputSchema) != string(raw) {
		t.Fatalf("raw schema with pre-existing telemetry was modified: %s", decorated.RawInputSchema)
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
			"user_intent": "test the wrap path",
		},
	}
	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped: %v", err)
	}
	if sawTel.UserIntent != "test the wrap path" {
		t.Errorf("inner handler didn't see user_intent on ctx: %+v", sawTel)
	}
	if _, ok := sawArgs["telemetry"]; ok {
		t.Errorf("inner handler still saw telemetry in args: %v", sawArgs)
	}
	if sawArgs["text"] != "hi" {
		t.Errorf("inner handler lost real args: %v", sawArgs)
	}
}
