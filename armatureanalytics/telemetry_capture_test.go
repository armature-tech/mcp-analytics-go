package armatureanalytics

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type contractVectors struct {
	Extraction []struct {
		Name            string          `json:"name"`
		Mode            string          `json:"mode"`
		Args            map[string]any  `json:"args"`
		ExpectArgs      map[string]any  `json:"expect_args"`
		ExpectTelemetry json.RawMessage `json:"expect_telemetry"`
	} `json:"extraction"`
	Sanitization []struct {
		Name   string          `json:"name"`
		Value  json.RawMessage `json:"value"`
		Expect json.RawMessage `json:"expect"`
	} `json:"sanitization"`
}

func loadContractVectors(t *testing.T) contractVectors {
	t.Helper()
	data, err := os.ReadFile("testdata/telemetry_contract_vectors.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var v contractVectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	return v
}

// expectedTelemetry holds only the V1 fields the shared vectors use.
type expectedTelemetry struct {
	UserTurn        int    `json:"user_turn"`
	UserIntent      string `json:"user_intent"`
	AgentThinking   string `json:"agent_thinking"`
	UserFrustration string `json:"user_frustration"`
}

// extractWithMode mirrors how the runtime applies modes: hooks skip extraction
// entirely for owned tools (recorder.go onBeforeAny), and RecordToolCall's
// capture gate zeroes telemetry in scrub deployments.
func extractWithMode(mode string, args map[string]any) (Telemetry, map[string]any) {
	switch mode {
	case "owned":
		return Telemetry{}, args
	case "scrub":
		_, cleaned := extractTelemetryFromArgs(args)
		return Telemetry{}, cleaned
	default:
		return extractTelemetryFromArgs(args)
	}
}

func TestContractExtractionVectors(t *testing.T) {
	for _, vec := range loadContractVectors(t).Extraction {
		t.Run(vec.Name, func(t *testing.T) {
			tel, cleaned := extractWithMode(vec.Mode, vec.Args)
			if !reflect.DeepEqual(cleaned, vec.ExpectArgs) {
				t.Fatalf("args: got %#v want %#v", cleaned, vec.ExpectArgs)
			}
			if string(vec.ExpectTelemetry) == "null" {
				if tel != (Telemetry{}) {
					t.Fatalf("telemetry: got %+v want zero", tel)
				}
				return
			}
			var expect expectedTelemetry
			if err := json.Unmarshal(vec.ExpectTelemetry, &expect); err != nil {
				t.Fatalf("decode expect_telemetry: %v", err)
			}
			if tel.UserTurn != expect.UserTurn ||
				tel.UserIntent != expect.UserIntent ||
				tel.AgentThinking != expect.AgentThinking ||
				tel.UserFrustration != expect.UserFrustration {
				t.Fatalf("telemetry: got %+v want %+v", tel, expect)
			}
			// The Go SDK carries legacy mirrors inside Telemetry; they must
			// always agree with the V1 fields after normalization.
			if tel.Intent != tel.UserIntent || tel.Context != tel.AgentThinking || tel.FrustrationLevel != tel.UserFrustration {
				t.Fatalf("legacy mirrors disagree: %+v", tel)
			}
		})
	}
}

func TestContractSanitizationVectors(t *testing.T) {
	for _, vec := range loadContractVectors(t).Sanitization {
		t.Run(vec.Name, func(t *testing.T) {
			var value, expect any
			if err := json.Unmarshal(vec.Value, &value); err != nil {
				t.Fatalf("decode value: %v", err)
			}
			if err := json.Unmarshal(vec.Expect, &expect); err != nil {
				t.Fatalf("decode expect: %v", err)
			}
			if got := SanitizeValue(value); !reflect.DeepEqual(got, expect) {
				t.Fatalf("got %#v want %#v", got, expect)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }

func TestCaptureOffDropsTelemetryAtChokePoint(t *testing.T) {
	sink := newRecordingSink(t)
	rec, err := NewRecorder(Config{
		APIKey:           "test-key",
		EndpointURL:      sink.srv.URL,
		CaptureTelemetry: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	rec.RecordToolCall(context.Background(), ToolCallInput{
		ToolName:   "search",
		Args:       map[string]any{"q": "x"},
		Telemetry:  Telemetry{UserIntent: "should never ship", UserTurn: 3},
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
	})
	if err := rec.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	events := sink.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	meta := events[0]["metadata"].(map[string]any)
	for _, key := range []string{"user_intent", "agent_thinking", "user_frustration", "user_turn", "intent", "context"} {
		if meta[key] != nil {
			t.Fatalf("metadata[%s] = %v, want nil", key, meta[key])
		}
	}
}

func TestTelemetryFieldMapExportsCustomerField(t *testing.T) {
	sink := newRecordingSink(t)
	rec, err := NewRecorder(Config{
		APIKey:            "test-key",
		EndpointURL:       sink.srv.URL,
		TelemetryFieldMap: map[string]string{"user_intent": "purpose"},
	})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close(context.Background()) })

	rec.RecordToolCall(context.Background(), ToolCallInput{
		ToolName:   "customer-tool",
		Args:       map[string]any{"purpose": "book a flight"},
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
	})
	if err := rec.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	meta := sink.Events()[0]["metadata"].(map[string]any)
	if meta["user_intent"] != "book a flight" {
		t.Fatalf("user_intent = %v, want mapped customer field", meta["user_intent"])
	}
	if meta["agent_thinking"] != nil {
		t.Fatalf("agent_thinking = %v, want nil", meta["agent_thinking"])
	}
}

func TestOwnedToolHooksLeaveCustomerTelemetryAlone(t *testing.T) {
	resetOwnedTelemetryToolsForTests()
	t.Cleanup(resetOwnedTelemetryToolsForTests)

	// Registering a tool whose schema declares telemetry records ownership.
	owned := mcp.NewTool("customer-tool", mcp.WithString("telemetry", mcp.Description("customer field")))
	s := server.NewMCPServer("t", "0")
	InstrumentTool(s, owned, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	if !IsTelemetryOwnedTool("customer-tool") {
		t.Fatal("ownership not recorded")
	}

	sink := newRecordingSink(t)
	rec := newSinkRecorder(t, sink)
	ctx := sessionCtx(newFakeSession("sess-1"))
	req := &mcp.CallToolRequest{}
	req.Params.Name = "customer-tool"
	req.Params.Arguments = map[string]any{
		"q":         "x",
		"telemetry": map[string]any{"user_intent": "customer data"},
	}
	rec.onBeforeAny(ctx, 1, mcp.MethodToolsCall, req)
	rec.onSuccess(ctx, 1, mcp.MethodToolsCall, req, &mcp.CallToolResult{})
	if err := rec.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var toolCall map[string]any
	for _, ev := range sink.Events() {
		if ev["kind"] == "tool_call" {
			toolCall = ev
		}
	}
	if toolCall == nil {
		t.Fatal("no tool_call event")
	}
	meta := toolCall["metadata"].(map[string]any)
	if meta["user_intent"] != nil {
		t.Fatalf("user_intent = %v, want nil (customer-owned field must not be interpreted)", meta["user_intent"])
	}
	// The preview keeps the customer's args exactly as the tool received them.
	if preview, _ := meta["input_preview"].(string); !strings.Contains(preview, "customer data") {
		t.Fatalf("input_preview lost customer-owned telemetry field: %q", preview)
	}
}

func TestDirectRecordToolCallForOwnedToolDropsTelemetry(t *testing.T) {
	resetOwnedTelemetryToolsForTests()
	t.Cleanup(resetOwnedTelemetryToolsForTests)
	MarkTelemetryOwnedTool("owned-direct")

	sink := newRecordingSink(t)
	rec := newSinkRecorder(t, sink)
	// An adapter calling RecordToolCall directly must not export telemetry
	// for a tool the customer owns — the choke point consults the registry.
	rec.RecordToolCall(context.Background(), ToolCallInput{
		ToolName:   "owned-direct",
		Args:       map[string]any{"telemetry": "customer value"},
		Telemetry:  Telemetry{UserIntent: "adapter-supplied"},
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
	})
	if err := rec.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	for _, ev := range sink.Events() {
		if ev["kind"] != "tool_call" {
			continue
		}
		if intent := ev["metadata"].(map[string]any)["user_intent"]; intent != nil {
			t.Fatalf("user_intent = %v, want nil for owned tool via direct record", intent)
		}
		return
	}
	t.Fatal("no tool_call event")
}

func TestSanitizeValueIsCycleSafe(t *testing.T) {
	m := map[string]any{"note": "keep"}
	m["self"] = m
	list := []any{"keep"}
	list = append(list, nil)
	list[1] = list
	m["list"] = list

	got, ok := SanitizeValue(m).(map[string]any)
	if !ok {
		t.Fatalf("unexpected type %T", got)
	}
	if got["self"] != "[circular]" {
		t.Fatalf(`got["self"] = %v, want "[circular]"`, got["self"])
	}
	gotList, ok := got["list"].([]any)
	if !ok || gotList[1] != "[circular]" {
		t.Fatalf(`got["list"] = %#v, want inner "[circular]"`, got["list"])
	}
	// Shared-but-acyclic values are still sanitized, not marked circular.
	shared := map[string]any{"blob": "QUFBQQ=="}
	twice := []any{shared, shared}
	gotTwice := SanitizeValue(twice).([]any)
	for i, item := range gotTwice {
		if item.(map[string]any)["blob"] != BinaryRemovedPlaceholder {
			t.Fatalf("shared item %d not sanitized: %#v", i, item)
		}
	}
}

func TestBuildToolCallEventRedactsAndFailsClosed(t *testing.T) {
	base64Payload := strings.Repeat("QUFB", 200)
	event := BuildToolCallEvent(ToolCallInput{
		ToolName: "upload",
		Args: map[string]any{
			"file": map[string]any{"type": "image", "data": base64Payload, "mimeType": "image/png"},
			"note": "secret-token-12345",
		},
		Result:     map[string]any{"stored": base64Payload},
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
		Redact: func(value any) any {
			data, _ := json.Marshal(value)
			var out any
			_ = json.Unmarshal([]byte(strings.ReplaceAll(string(data), "secret-token-12345", "[redacted]")), &out)
			return out
		},
	})
	preview := event.Metadata["input_preview"].(string)
	if !strings.Contains(preview, BinaryRemovedPlaceholder) {
		t.Fatalf("image data not sanitized: %q", preview)
	}
	if strings.Contains(preview, "secret-token-12345") || !strings.Contains(preview, "[redacted]") {
		t.Fatalf("redact hook not applied: %q", preview)
	}
	if !strings.Contains(*event.ScriptSource, "[redacted]") {
		t.Fatalf("script_source not redacted: %q", *event.ScriptSource)
	}
	if !strings.Contains(*event.ResultPreview, Base64RemovedPlaceholder) {
		t.Fatalf("result base64 not sanitized: %q", *event.ResultPreview)
	}

	panicking := BuildToolCallEvent(ToolCallInput{
		ToolName:   "upload",
		Args:       map[string]any{"secret": "leak me not"},
		Result:     map[string]any{"alsoSecret": true},
		Err:        errRedact{"failed with leak me not"},
		Telemetry:  Telemetry{UserIntent: "quotes the user"},
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
		Redact:     func(any) any { panic("boom") },
	})
	if got := panicking.Metadata["input_preview"].(string); got != `"`+RedactionFailedPlaceholder+`"` {
		t.Fatalf("input_preview = %q, want fail-closed placeholder", got)
	}
	if got := *panicking.ResultPreview; got != `"`+RedactionFailedPlaceholder+`"` {
		t.Fatalf("result_preview = %q, want fail-closed placeholder", got)
	}
	if *panicking.Error != RedactionFailedPlaceholder {
		t.Fatalf("error = %q, want fail-closed placeholder", *panicking.Error)
	}
	if panicking.Metadata["user_intent"] != nil {
		t.Fatalf("user_intent = %v, want nil after fail-closed telemetry redaction", panicking.Metadata["user_intent"])
	}
	if strings.Contains(*panicking.ScriptSource, "leak me not") {
		t.Fatalf("script_source leaked payload: %q", *panicking.ScriptSource)
	}
}

type errRedact struct{ msg string }

func (e errRedact) Error() string { return e.msg }

func TestInstrumentToolWithConfigCaptureOffKeepsSchema(t *testing.T) {
	resetOwnedTelemetryToolsForTests()
	t.Cleanup(resetOwnedTelemetryToolsForTests)

	tool := mcp.NewTool("search", mcp.WithString("q", mcp.Description("query")))
	originalDescription := tool.Description
	s := server.NewMCPServer("t", "0")
	InstrumentToolWithConfig(Config{CaptureTelemetry: boolPtr(false)}, s, tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if _, exists := req.GetArguments()["telemetry"]; exists {
			t.Fatal("telemetry not scrubbed from handler args")
		}
		return &mcp.CallToolResult{}, nil
	})

	// The registered tool must advertise the original schema and description.
	listed := listTool(t, s, "search")
	if _, exists := listed.InputSchema.Properties["telemetry"]; exists {
		t.Fatal("telemetry injected despite capture off")
	}
	if listed.Description != originalDescription {
		t.Fatalf("description changed: %q", listed.Description)
	}
}

// listTool fetches a registered tool definition from the server.
func listTool(t *testing.T, s *server.MCPServer, name string) mcp.Tool {
	t.Helper()
	msg := s.HandleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal tools/list response: %v", err)
	}
	var resp struct {
		Result struct {
			Tools []mcp.Tool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("decode tools/list response: %v", err)
	}
	for _, tool := range resp.Result.Tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return mcp.Tool{}
}
