package armatureanalytics

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCallPurposePrecedenceAndCachedClients(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input map[string]any
		want  string
	}{
		{"public", map[string]any{"call_purpose": "Read the requested status"}, "Read the requested status"},
		{"new_wins", map[string]any{"call_purpose": "public action", "agent_thinking": "cached", "context": "legacy"}, "public action"},
		{"empty_wins", map[string]any{"call_purpose": "", "agent_thinking": "cached", "context": "legacy"}, ""},
		{"wrong_type_falls_back", map[string]any{"call_purpose": 7, "agent_thinking": "cached"}, "cached"},
		{"cached_empty_wins", map[string]any{"agent_thinking": "", "context": "legacy"}, ""},
		{"legacy", map[string]any{"context": "legacy"}, "legacy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, encoded := range []bool{false, true} {
				var block any = tc.input
				if encoded {
					data, _ := json.Marshal(block)
					block = string(data)
				}
				tel, args := extractTelemetryFromArgs(map[string]any{"telemetry": block, "id": "business-id"})
				for i := 0; i < 2; i++ {
					if tel.CallPurpose != "" || tel.AgentThinking != tc.want || tel.Context != tc.want {
						t.Fatalf("normalization %d = %+v, want %q", i, tel, tc.want)
					}
					tel = NormalizeTelemetry(tel)
				}
				if !reflect.DeepEqual(args, map[string]any{"id": "business-id"}) {
					t.Fatalf("business arguments changed: %v", args)
				}
			}
		})
	}
}

func TestCallPurposeJSONEmptySuppressesFallbackMapping(t *testing.T) {
	var tel Telemetry
	if err := json.Unmarshal([]byte(`{"call_purpose":"","agent_thinking":"stale","context":"legacy"}`), &tel); err != nil {
		t.Fatal(err)
	}

	tel = applyTelemetryFieldMap(NormalizeTelemetry(tel), map[string]any{"purpose": "mapped"}, map[string]string{"call_purpose": "purpose"})
	if got := NormalizeTelemetry(tel); got.AgentThinking != "" || got.Context != "" {
		t.Fatalf("empty public value replaced: %+v", got)
	}
}

func TestCallPurposeJSONIgnoresInvalidAliasTypes(t *testing.T) {
	for _, invalid := range []string{`7`, `null`, `true`, `{}`, `[]`, `1e999`} {
		var telemetry Telemetry
		data := `{"user_intent":"valid goal","call_purpose":` + invalid + `,"agent_thinking":"legacy action","user_frustration":"low"}`
		if err := json.Unmarshal([]byte(data), &telemetry); err != nil {
			t.Fatalf("invalid alias %s rejected valid context: %v", invalid, err)
		}
		got := NormalizeTelemetry(telemetry)
		if got.UserIntent != "valid goal" || got.AgentThinking != "legacy action" || got.Context != "legacy action" || got.UserFrustration != "low" {
			t.Fatalf("invalid alias %s lost valid context: %+v", invalid, got)
		}
	}
	var telemetry Telemetry
	if err := json.Unmarshal([]byte(`{"call_purpose":7,"user_intent":42}`), &telemetry); err == nil {
		t.Fatal("existing typed field validation must stay strict")
	}
}

func TestCallPurposeFieldMapping(t *testing.T) {
	args := map[string]any{"purpose": "Read the requested status", "cached": "legacy mapping"}
	mapping := map[string]string{"call_purpose": "purpose", "agent_thinking": "cached"}
	got := NormalizeTelemetry(applyTelemetryFieldMap(Telemetry{}, args, mapping))
	if got.AgentThinking != "Read the requested status" {
		t.Fatalf("public mapping lost: %+v", got)
	}

	args["purpose"] = ""
	got = NormalizeTelemetry(applyTelemetryFieldMap(Telemetry{}, args, mapping))
	if got.AgentThinking != "legacy mapping" {
		t.Fatalf("empty public mapping should fall back: %+v", got)
	}
	if got := applyTelemetryFieldMap(Telemetry{}, args, map[string]string{"call_purpose": "purpose"}); got != (Telemetry{}) {
		t.Fatalf("empty public mapping should be ignored: %+v", got)
	}
	args["purpose"] = 42
	got = NormalizeTelemetry(applyTelemetryFieldMap(Telemetry{}, args, mapping))
	if got.AgentThinking != "legacy mapping" {
		t.Fatalf("wrong-typed public mapping should fall back: %+v", got)
	}
	got = NormalizeTelemetry(applyTelemetryFieldMap(Telemetry{CallPurpose: "explicit"}, args, mapping))
	if got.AgentThinking != "explicit" {
		t.Fatalf("explicit telemetry replaced: %+v", got)
	}
	if args["purpose"] != 42 || args["cached"] != "legacy mapping" {
		t.Fatalf("mapped arguments changed: %v", args)
	}
}

func TestCallPurposeEmitsSanitizedHistoricalStorage(t *testing.T) {
	now := time.Now()
	event := BuildToolCallEvent(ToolCallInput{
		ToolName: "lookup", StartedAt: now, FinishedAt: now,
		Telemetry: Telemetry{CallPurpose: "Read status password=hunter2", AgentThinking: "stale"},
	})
	for _, key := range []string{"agent_thinking", "context"} {
		got, _ := event.Metadata[key].(string)
		if got == "" || strings.Contains(got, "hunter2") || strings.Contains(got, "stale") {
			t.Fatalf("unsafe metadata %s: %q", key, got)
		}
	}
	if _, exists := event.Metadata["call_purpose"]; exists {
		t.Fatal("wire storage field names changed")
	}
}

func TestCallPurposeKeepsLegacyRedactionEffective(t *testing.T) {
	now := time.Now()
	event := BuildToolCallEvent(ToolCallInput{
		ToolName: "lookup", StartedAt: now, FinishedAt: now,
		Telemetry: Telemetry{CallPurpose: "private business detail"},
		Redact: func(value any) any {
			if telemetry, ok := value.(Telemetry); ok {
				telemetry.AgentThinking = "redacted action"
				return telemetry
			}
			return value
		},
	})
	for _, key := range []string{"agent_thinking", "context"} {
		if event.Metadata[key] != "redacted action" {
			t.Fatalf("public alias restored pre-redaction text in %s: %v", key, event.Metadata[key])
		}
	}
}

func TestCallPurposeInvalidAliasFromMapRedactionKeepsValidContext(t *testing.T) {
	now := time.Now()
	event := BuildToolCallEvent(ToolCallInput{
		ToolName: "lookup", StartedAt: now, FinishedAt: now,
		Telemetry: Telemetry{CallPurpose: "initial action"},
		Redact: func(value any) any {
			if _, ok := value.(Telemetry); ok {
				return map[string]any{
					"user_intent": "valid goal", "call_purpose": 7,
					"agent_thinking": "redacted legacy action", "user_frustration": "low",
				}
			}
			return value
		},
	})
	for key, want := range map[string]string{
		"user_intent": "valid goal", "agent_thinking": "redacted legacy action",
		"context": "redacted legacy action", "user_frustration": "low",
	} {
		if event.Metadata[key] != want {
			t.Fatalf("invalid alias discarded %s: got %v, want %s", key, event.Metadata[key], want)
		}
	}
}

func TestPublicTelemetrySchemaIsOptional(t *testing.T) {
	schema := TelemetryInputSchema()
	props := schema["properties"].(map[string]any)
	if len(props) != 3 || props["agent_thinking"] != nil || props["context"] != nil {
		t.Fatalf("unexpected public fields: %v", props)
	}
	if _, exists := schema["required"]; exists {
		t.Fatal("telemetry fields must remain optional")
	}
	for _, name := range []string{"user_intent", "call_purpose", "user_frustration"} {
		field := props[name].(map[string]any)
		if field["type"] != "string" || field["enum"] != nil {
			t.Fatalf("field is not a permissive string: %s", name)
		}
	}
}

func TestLegacyHintMigrationPreservesCustomerText(t *testing.T) {
	for _, hint := range legacyTelemetryHints {
		for _, opts := range []HintOptions{{}, {RequestCapability: true}} {
			for _, suffix := range []string{hint, hint + " " + requestCapabilitySentence} {
				base := "Recherche les documents demandés."
				want := AppendTelemetryHintWithOptions(base, opts)
				if got := AppendTelemetryHintWithOptions(base+suffix, opts); got != want {
					t.Fatalf("suffix migration mismatch: %q", got)
				}
				if got := AppendTelemetryHintWithOptions(strings.TrimLeft(suffix, "\n"), opts); got != AppendTelemetryHintWithOptions("", opts) {
					t.Fatalf("standalone migration mismatch: %q", got)
				}
			}
			// A known sentence in the middle is customer prose, not a suffix.
			embedded := "Customer example:" + hint + "\nKeep this example."
			if got := AppendTelemetryHintWithOptions(embedded, opts); !strings.HasPrefix(got, embedded) {
				t.Fatalf("customer prose changed: %q", got)
			}
			longBase := strings.Repeat("é", MaxToolDescriptionLength/2)
			if got := AppendTelemetryHintWithOptions(longBase+hint, opts); got != longBase {
				t.Fatalf("old hint survived length fallback or customer text changed")
			}
		}
	}
}

func TestLegacyHintMigrationRemovesStackedSuffixes(t *testing.T) {
	for _, first := range legacyTelemetryHints {
		for _, second := range legacyTelemetryHints {
			for _, opts := range []HintOptions{{}, {RequestCapability: true}} {
				stacked := first + " " + requestCapabilitySentence + second
				for _, base := range []string{"", "Recherche les documents demandés.", strings.Repeat("é", MaxToolDescriptionLength/2)} {
					input := base + stacked
					if base == "" {
						input = strings.TrimLeft(input, "\n")
					}
					want := AppendTelemetryHintWithOptions(base, opts)
					got := AppendTelemetryHintWithOptions(input, opts)
					if got != want || strings.Contains(got, "agent_thinking") {
						t.Fatalf("stacked SDK suffix survived migration: %q", got)
					}
					if again := AppendTelemetryHintWithOptions(got, opts); again != got {
						t.Fatalf("migrated hint is not idempotent: %q", again)
					}
				}
			}
		}
	}
}
