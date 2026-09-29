package armatureanalytics

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
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
	if tel.UserTurn != 0 {
		t.Errorf("cached UserTurn must be ignored, got %d", tel.UserTurn)
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

func TestExtractTelemetryFromArgs_IgnoresCachedUserTurn(t *testing.T) {
	for _, cached := range []any{1.9, float64(0), float64(-1), float64(2)} {
		args := map[string]any{"telemetry": map[string]any{"user_turn": cached, "user_frustration": "annoyed"}}
		tel, _ := extractTelemetryFromArgs(args)
		if tel.UserTurn != 0 {
			t.Errorf("user_turn %v should be ignored, got %d", cached, tel.UserTurn)
		}
		if tel.UserFrustration != "" {
			t.Errorf("off-spec frustration should be dropped, got %q", tel.UserFrustration)
		}
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
	// Known historical SDK suffixes migrate to the public task-context hint.
	for _, hint := range legacyTelemetryHints {
		old := "Echoes." + hint
		if got := AppendTelemetryHint(old); got != once {
			t.Errorf("historical hint was not migrated: %q", got)
		}
	}
	if AppendTelemetryHint("") == "" {
		t.Errorf("empty description should become the hint")
	}
	// A description already carrying the request_capability-aware hint (e.g.
	// from an earlier AppendTelemetryHintWithOptions call) is also recognized,
	// even by the config-agnostic function.
	withCapability := "Echoes." + telemetryDescriptionHintWithCapability
	if AppendTelemetryHint(withCapability) != withCapability {
		t.Errorf("request_capability-hinted description modified")
	}
}

// TestAppendTelemetryHintWithOptions_RequestCapabilityEnabled covers (a) from
// TELEMETRY-CONTRACT.md's hint-decoration matrix: request_capability enabled
// (the nil-Config default, same as an explicit pointer to true) appends the
// hint that also points agents at request_capability.
func TestAppendTelemetryHintWithOptions_RequestCapabilityEnabled(t *testing.T) {
	want := "Echoes." + telemetryDescriptionHintWithCapability
	if got := AppendTelemetryHintWithOptions("Echoes.", HintOptions{RequestCapability: true}); got != want {
		t.Fatalf("nil-RequestCapability hint = %q, want %q", got, want)
	}
	if got := AppendTelemetryHintWithOptions("Echoes.", HintOptions{RequestCapability: true}); got != want {
		t.Fatalf("explicit-on hint = %q, want %q", got, want)
	}
}

// The hint only names request_capability on a server that lists it. A config
// with a delivery path is not enough: a standalone server never registered it.
func TestInstrumentToolNamesRequestCapabilityOnlyWhereRegistered(t *testing.T) {
	handler := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	}
	standalone := server.NewMCPServer("standalone", "0.0.1")
	InstrumentToolWithConfig(Config{APIKey: "k"}, standalone, mcp.NewTool("weather", mcp.WithDescription("Weather.")), handler)
	if got := standalone.GetTool("weather").Tool.Description; got != "Weather."+telemetryDescriptionHint {
		t.Fatalf("standalone server hint = %q, want the plain hint", got)
	}

	constructed, shutdown := NewMCPServerWithConfig("constructed", "0.0.1", Config{Emit: func(context.Context, Batch) error { return nil }})
	defer func() { _ = shutdown(context.Background()) }()
	InstrumentTool(constructed, mcp.NewTool("weather", mcp.WithDescription("Weather.")), handler)
	if got := constructed.GetTool("weather").Tool.Description; got != "Weather."+telemetryDescriptionHintWithCapability {
		t.Fatalf("constructor server hint = %q, want the request_capability hint", got)
	}

	disabled, shutdownDisabled := NewMCPServerWithConfig("disabled", "0.0.1", Config{Emit: func(context.Context, Batch) error { return nil }, RequestCapability: boolPtr(false)})
	defer func() { _ = shutdownDisabled(context.Background()) }()
	InstrumentTool(disabled, mcp.NewTool("weather", mcp.WithDescription("Weather.")), handler)
	if got := disabled.GetTool("weather").Tool.Description; got != "Weather."+telemetryDescriptionHint {
		t.Fatalf("request_capability-off server hint = %q, want the plain hint", got)
	}
}

// TestAppendTelemetryHintWithOptions_RequestCapabilityDisabled covers (b):
// request_capability explicitly disabled keeps today's hint, byte-identical.
func TestAppendTelemetryHintWithOptions_RequestCapabilityDisabled(t *testing.T) {
	got := AppendTelemetryHintWithOptions("Echoes.", HintOptions{})
	want := AppendTelemetryHint("Echoes.")
	if got != want {
		t.Fatalf("request_capability-off hint = %q, want byte-identical current hint %q", got, want)
	}
	if got != "Echoes."+telemetryDescriptionHint {
		t.Fatalf("hint text drifted from telemetryDescriptionHint: %q", got)
	}
}

// TestAppendTelemetryHintWithOptions_Idempotent covers (c): a description
// already carrying any recognized hint is returned unchanged, regardless of
// which Config produced it or is passed on the next call.
func TestAppendTelemetryHintWithOptions_Idempotent(t *testing.T) {
	enabled := HintOptions{RequestCapability: true}
	disabled := HintOptions{}

	once := AppendTelemetryHintWithOptions("Echoes.", enabled)
	if AppendTelemetryHintWithOptions(once, enabled) != once {
		t.Errorf("request_capability hint appended twice")
	}
	// Flipping cfg after the hint is already present must not re-decorate.
	if AppendTelemetryHintWithOptions(once, disabled) != once {
		t.Errorf("hint re-appended after cfg changed from enabled to disabled")
	}
	plain := AppendTelemetryHintWithOptions("Echoes.", disabled)
	if AppendTelemetryHintWithOptions(plain, enabled) != plain {
		t.Errorf("hint re-appended after cfg changed from disabled to enabled")
	}
	// The config-agnostic AppendTelemetryHint's output is recognized too.
	legacyPath := AppendTelemetryHint("Echoes.")
	if AppendTelemetryHintWithOptions(legacyPath, enabled) != legacyPath {
		t.Errorf("config-aware append re-decorated a plain-hinted description")
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
	if _, ok := props["user_turn"]; ok {
		t.Errorf("removed user_turn sub-property still advertised")
	}
	for _, key := range []string{"user_intent", "call_purpose", "user_frustration"} {
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

func TestAppendTelemetryHintFallsBackToTheTelemetrySentenceThenNothing(t *testing.T) {
	enabled := HintOptions{RequestCapability: true}
	disabled := HintOptions{}
	for _, tc := range []struct {
		cfg  HintOptions
		hint string
	}{{enabled, telemetryDescriptionHintWithCapability}, {disabled, telemetryDescriptionHint}} {
		fullFits := strings.Repeat("a", MaxToolDescriptionLength-len(tc.hint))
		if got := AppendTelemetryHintWithOptions(fullFits, tc.cfg); got != fullFits+tc.hint {
			t.Fatalf("full hint should fit, got %d bytes", len(got))
		}
		sentenceOnly := fullFits + "a"
		want := sentenceOnly
		if len(tc.hint) > len(telemetrySentenceHint) {
			want += telemetrySentenceHint
		}
		if got := AppendTelemetryHintWithOptions(sentenceOnly, tc.cfg); got != want {
			t.Fatalf("unexpected fallback: %q", got[len(sentenceOnly):])
		}
		sentenceFits := strings.Repeat("a", MaxToolDescriptionLength-len(telemetrySentenceHint))
		if got := AppendTelemetryHintWithOptions(sentenceFits, tc.cfg); len(got) != MaxToolDescriptionLength {
			t.Fatalf("telemetry sentence should fit exactly, got %d bytes", len(got))
		}
		nothingFits := sentenceFits + "a"
		if got := AppendTelemetryHintWithOptions(nothingFits, tc.cfg); got != nothingFits {
			t.Fatalf("description should be unchanged, got %d bytes", len(got))
		}
	}
	// Length is counted in UTF-8 bytes: "é" is one character but two bytes.
	accented := strings.Repeat("é", (MaxToolDescriptionLength-len(telemetrySentenceHint))/2)
	if got := AppendTelemetryHintWithOptions(accented, enabled); got != accented+telemetrySentenceHint {
		t.Fatalf("UTF-8 description should get the telemetry hint only")
	}
	// The exported config-less helper applies the same guard.
	tooLong := strings.Repeat("a", MaxToolDescriptionLength)
	if got := AppendTelemetryHint(tooLong); got != tooLong {
		t.Fatalf("AppendTelemetryHint must not exceed the limit")
	}
}

func TestAppendTelemetryHintIsIdempotentAfterFallback(t *testing.T) {
	enabled := HintOptions{RequestCapability: true}
	for _, n := range []int{10, 900, 950, 1000} {
		once := AppendTelemetryHintWithOptions(strings.Repeat("a", n), enabled)
		if AppendTelemetryHintWithOptions(once, enabled) != once {
			t.Fatalf("second pass changed a %d-byte description", n)
		}
		if AppendTelemetryHintWithOptions(once, HintOptions{}) != once {
			t.Fatalf("second pass under another config changed a %d-byte description", n)
		}
	}
}

func TestAppendTelemetryHintSkipsTheRequestCapabilitySentenceTheCustomerWrote(t *testing.T) {
	description := "Look up a customer. " + requestCapabilitySentence
	got := AppendTelemetryHintWithOptions(description, HintOptions{RequestCapability: true})
	if got != description+telemetrySentenceHint {
		t.Fatalf("expected only the telemetry sentence, got %q", got)
	}
}

func TestAppendTelemetryHintWarnsOncePerToolWhenShortened(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()
	tooLong := strings.Repeat("x", MaxToolDescriptionLength-10)
	shortened := strings.Repeat("y", MaxToolDescriptionLength-len(telemetrySentenceHint))
	for i := 0; i < 2; i++ {
		AppendTelemetryHintWithOptions(tooLong, HintOptions{RequestCapability: true, ToolName: "go_long_description_tool"})
		AppendTelemetryHintWithOptions(shortened, HintOptions{RequestCapability: true, ToolName: "go_shortened_hint_tool"})
	}
	want := `[mcp-analytics] Tool "go_long_description_tool" description is too long to append the Armature telemetry hint without exceeding 1024 characters; leaving it unchanged. Telemetry is still collected.` + "\n" +
		`[mcp-analytics] Tool "go_shortened_hint_tool" description is too long for the full Armature telemetry hint within 1024 characters; appended only the telemetry sentence.` + "\n"
	if buf.String() != want {
		t.Fatalf("unexpected warnings:\n%s", buf.String())
	}
}

func TestInstrumentToolKeepsTelemetrySchemaWhenTheHintIsDropped(t *testing.T) {
	s := server.NewMCPServer("long", "0.0.1")
	description := strings.Repeat("z", MaxToolDescriptionLength)
	tool := mcp.NewTool("go_schema_kept_tool", mcp.WithDescription(description))
	InstrumentToolWithConfig(Config{APIKey: "k"}, s, tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	registered := s.GetTool("go_schema_kept_tool")
	if registered == nil {
		t.Fatal("tool not registered")
	}
	if registered.Tool.Description != description {
		t.Fatalf("description changed: %d bytes", len(registered.Tool.Description))
	}
	if _, ok := registered.Tool.InputSchema.Properties["telemetry"]; !ok {
		t.Fatal("telemetry property should still be advertised")
	}
}
