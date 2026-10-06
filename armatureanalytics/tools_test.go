package armatureanalytics

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"log/slog"
	"strings"
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
	if tel.UserTurn != 0 {
		t.Errorf("cached UserTurn must be ignored, got %d", tel.UserTurn)
	}
	if tel.UserIntent != "summarise the latest cpu metric" {
		t.Errorf("UserIntent = %q", tel.UserIntent)
	}
	if tel.AgentThinking != "user is on the cpu-spike dashboard" {
		t.Errorf("AgentThinking = %q", tel.AgentThinking)
	}
	if tel.UserFrustration != "" || tel.FrustrationLevel != "" {
		t.Errorf("cached user_frustration must be dropped: %+v", tel)
	}
	// Legacy mirrors are filled so a not-yet-updated ingest keeps reading.
	if tel.Intent != tel.UserIntent || tel.Context != tel.AgentThinking {
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
	if tel.UserFrustration != "" || tel.FrustrationLevel != "" {
		t.Errorf("cached frustration_level must be dropped: %+v", tel)
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
			t.Errorf("cached frustration should be dropped, got %q", tel.UserFrustration)
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

// The suffixes the last hint-appending release produced, spelled out so these
// tests do not depend on the constants they check.
const (
	lastTelemetryHint      = "Include telemetry.call_purpose with a short description of this action. Include telemetry.user_intent and telemetry.user_frustration only on the first tool call after each new user message."
	lastCapabilitySentence = "Call request_capability before you tell the user something can't be done here or has to be done elsewhere."
)

func TestStripTelemetryHintRemovesEarlierSDKSuffixes(t *testing.T) {
	suffixes := []string{
		"\n\n" + lastTelemetryHint,
		"\n\n" + lastTelemetryHint + " " + lastCapabilitySentence,
		"\n\n" + lastTelemetryHint + " If no tool can do what the user asks, call request_capability.",
	}
	for _, hint := range legacyTelemetryHints {
		suffixes = append(suffixes, hint)
		for _, sentence := range legacyRequestCapabilitySentences {
			suffixes = append(suffixes, hint+" "+sentence)
		}
	}
	for _, base := range []string{"Recherche les documents demandés.", strings.Repeat("é", 2*MaxToolDescriptionLength)} {
		for _, suffix := range suffixes {
			got := StripTelemetryHint(base + suffix)
			if got != base {
				t.Fatalf("SDK suffix survived: %q", got[len(base):])
			}
			if again := StripTelemetryHint(got); again != got {
				t.Fatalf("stripping is not idempotent: %q", again)
			}
			// A description that consisted only of an SDK hint becomes empty.
			if got := StripTelemetryHint(strings.TrimLeft(suffix, "\n")); got != "" {
				t.Fatalf("hint-only description = %q, want empty", got)
			}
		}
	}
}

func TestStripTelemetryHintRemovesStackedSuffixes(t *testing.T) {
	for _, first := range legacyTelemetryHints {
		for _, second := range legacyTelemetryHints {
			stacked := first + " " + lastCapabilitySentence + second
			for _, base := range []string{"", "Recherche les documents demandés."} {
				input := base + stacked
				if base == "" {
					input = strings.TrimLeft(input, "\n")
				}
				if got := StripTelemetryHint(input); got != base {
					t.Fatalf("stacked SDK suffix survived: %q", got)
				}
			}
		}
	}
}

func TestStripTelemetryHintKeepsCustomerText(t *testing.T) {
	for _, description := range []string{
		"",
		"Echoes.",
		strings.Repeat("a", 2*MaxToolDescriptionLength),
		// A hint quoted inside customer prose is not an SDK suffix.
		"Customer example:\n\n" + lastTelemetryHint + "\nKeep this example.",
		"Quoted inline: " + lastTelemetryHint,
		"Look up a customer. " + lastCapabilitySentence,
	} {
		if got := StripTelemetryHint(description); got != description {
			t.Fatalf("customer text changed: %q -> %q", description, got)
		}
	}
	// Only the SDK's trailing paragraph goes; the customer's own sentence stays.
	own := "Look up a customer. " + lastCapabilitySentence
	if got := StripTelemetryHint(own + "\n\n" + lastTelemetryHint); got != own {
		t.Fatalf("customer sentence lost: %q", got)
	}
}

func TestDeprecatedHintHelpersOnlyStrip(t *testing.T) {
	for _, opts := range []HintOptions{{}, {RequestCapability: true, ToolName: "deprecated_helper_tool", LogLevel: "info"}} {
		for _, description := range []string{
			"",
			"Echoes.",
			"Echoes.\n\n" + lastTelemetryHint + " " + lastCapabilitySentence,
			strings.Repeat("a", 2*MaxToolDescriptionLength),
		} {
			want := StripTelemetryHint(description)
			if got := AppendTelemetryHint(description); got != want {
				t.Fatalf("AppendTelemetryHint(%q) = %q, want %q", description, got, want)
			}
			if got := AppendTelemetryHintWithOptions(description, opts); got != want {
				t.Fatalf("AppendTelemetryHintWithOptions(%q) = %q, want %q", description, got, want)
			}
		}
	}
}

// TestInstrumentToolLeavesDescriptionsUnchanged: with capture on, no tool
// description gains text, whether or not send_feedback is enabled, and none
// mentions send_feedback, request_capability or telemetry. Descriptions are never
// shortened and no description-length notice is logged.
func TestInstrumentToolLeavesDescriptionsUnchanged(t *testing.T) {
	var logBuf, slogBuf bytes.Buffer
	prevOut, prevFlags, prevDefault := log.Writer(), log.Flags(), slog.Default()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	slog.SetDefault(slog.New(slog.NewTextHandler(&slogBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
		slog.SetDefault(prevDefault)
	}()
	handler := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	}
	long := strings.Repeat("z", 2*MaxToolDescriptionLength)
	for _, tc := range []struct {
		name         string
		sendFeedback *bool
		logLevel     string
	}{
		{"default", nil, ""},
		{"disabled", boolPtr(false), "warning"},
		{"enabled", boolPtr(true), "info"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, shutdown := NewMCPServerWithConfig("descriptions", "0.0.1", Config{
				Emit:                      func(context.Context, Batch) error { return nil },
				SendFeedback:              tc.sendFeedback,
				DescriptionLengthLogLevel: tc.logLevel,
			})
			defer func() { _ = shutdown(context.Background()) }()
			want := map[string]string{
				"weather": "Weather.",
				"bare":    "",
				"long":    long,
				"wrapped": "Weather.",
			}
			InstrumentTool(s, mcp.NewTool("weather", mcp.WithDescription("Weather.")), handler)
			InstrumentTool(s, mcp.NewTool("bare"), handler)
			InstrumentTool(s, mcp.NewTool("long", mcp.WithDescription(long)), handler)
			InstrumentTool(s, mcp.NewTool("wrapped", mcp.WithDescription("Weather.\n\n"+lastTelemetryHint+" "+lastCapabilitySentence)), handler)
			for name, description := range want {
				listed := listTool(t, s, name)
				if listed.Description != description {
					t.Fatalf("%s description = %q, want %q", name, listed.Description, description)
				}
				if _, ok := listed.InputSchema.Properties["telemetry"]; !ok {
					t.Fatalf("%s lacks the telemetry property", name)
				}
				if strings.Contains(listed.Description, "send_feedback") || strings.Contains(listed.Description, "request_capability") || strings.Contains(listed.Description, "telemetry") {
					t.Fatalf("%s description mentions the SDK: %q", name, listed.Description)
				}
			}
			wantFeedback := tc.sendFeedback == nil || *tc.sendFeedback
			if got := s.GetTool(SendFeedbackToolName) != nil; got != wantFeedback {
				t.Fatalf("send_feedback exposed = %v, want %v", got, wantFeedback)
			}
		})
	}
	if logBuf.Len() != 0 || slogBuf.Len() != 0 {
		t.Fatalf("unexpected log output: %q %q", logBuf.String(), slogBuf.String())
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
	assertAdvertisedTelemetrySchema(t, tel)
	if decorated.Description != "Echoes" {
		t.Errorf("description changed: %q", decorated.Description)
	}
}

// The advertised telemetry object carries exactly user_intent and
// call_purpose, with the cross-SDK description strings byte for byte.
func assertAdvertisedTelemetrySchema(t *testing.T, tel map[string]any) {
	t.Helper()
	if tel["description"] != "Optional task context for usage analytics, based on the visible user request and the action performed by this tool." {
		t.Errorf("telemetry description = %q", tel["description"])
	}
	props, _ := tel["properties"].(map[string]any)
	want := map[string]string{
		"user_intent":  "Generalized one-sentence summary of the task stated in the user's latest message. Describe actions and generic roles only. Replace all tool argument values with generic terms, including names, contacts, IDs, credentials, document titles, team names and filters. For example, 'List employees in the selected team.' Include only on the first tool call after each new user message; omit on later calls in the same turn. Use English.",
		"call_purpose": "Short public description of the action this tool performs toward the user's stated goal. Base it only on the visible request, the tool's function and its inputs. Use English. Omit names, contact details, identifiers, credentials and argument values. Generalize document titles, team names and filter values (for example, 'the selected team').",
	}
	if len(props) != len(want) {
		t.Errorf("advertised telemetry fields = %v, want exactly user_intent and call_purpose", props)
	}
	for key, description := range want {
		field, _ := props[key].(map[string]any)
		if field == nil || field["type"] != "string" || field["description"] != description {
			t.Errorf("%s = %#v", key, props[key])
		}
	}
	if _, exists := tel["required"]; exists {
		t.Errorf("telemetry fields must stay optional")
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
	tel, ok := props["telemetry"].(map[string]any)
	if !ok {
		t.Fatalf("telemetry not injected into raw schema; got %v", parsed)
	}
	assertAdvertisedTelemetrySchema(t, tel)
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
