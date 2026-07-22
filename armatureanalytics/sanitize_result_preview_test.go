package armatureanalytics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Regression tests for mcp-tester#1381 and #1382: tool results captured as
// *mcp.CallToolResult must be previewed through their JSON wire format, with
// base64 payloads removed even when the result duplicates them inside its
// serialized text content.

func structuredFixtureResult(t *testing.T, title string) *mcp.CallToolResult {
	t.Helper()
	payload := map[string]any{
		"ok": true,
		"follow_up": map[string]any{
			"customer_id": "cust_acme",
			"title":       title,
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return mcp.NewToolResultStructured(payload, string(raw))
}

func TestResultPreviewUsesWireFormatNotStructFields(t *testing.T) {
	event := BuildToolCallEvent(ToolCallInput{
		ToolName:   "create_follow_up",
		Args:       map[string]any{"customer_id": "cust_acme"},
		Result:     structuredFixtureResult(t, "Send pricing"),
		SessionID:  "mcp_test_v_1_00000000-0000-4000-8000-000000000000",
		StartedAt:  time.Unix(0, 0),
		FinishedAt: time.Unix(1, 0),
	})
	if event.ResultPreview == nil {
		t.Fatal("expected a result preview")
	}
	preview := *event.ResultPreview
	for _, noise := range []string{`"Result"`, `"Annotated"`} {
		if strings.Contains(preview, noise) {
			t.Fatalf("preview leaks struct internals %s: %s", noise, preview)
		}
	}
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(preview), &decoded); err != nil {
		t.Fatalf("preview is not wire-shaped JSON: %v\n%s", err, preview)
	}
	if _, ok := decoded["content"]; !ok {
		t.Fatalf("preview missing wire content field: %s", preview)
	}
	if _, ok := decoded["structuredContent"]; !ok {
		t.Fatalf("preview missing structuredContent: %s", preview)
	}
}

func TestResultPreviewRemovesBase64FromTextContent(t *testing.T) {
	blob := strings.Repeat("QkJC", 200) // 800 chars of base64 alphabet
	event := BuildToolCallEvent(ToolCallInput{
		ToolName:   "create_follow_up",
		Args:       map[string]any{"customer_id": "cust_acme", "title": blob},
		Result:     structuredFixtureResult(t, blob),
		SessionID:  "mcp_test_v_1_00000000-0000-4000-8000-000000000000",
		StartedAt:  time.Unix(0, 0),
		FinishedAt: time.Unix(1, 0),
	})
	if event.ResultPreview == nil {
		t.Fatal("expected a result preview")
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), blob[:64]) {
		t.Fatalf("base64 payload survived somewhere in the event: %s", raw)
	}
	if !strings.Contains(*event.ResultPreview, Base64RemovedPlaceholder) {
		t.Fatalf("expected %q in result preview: %s", Base64RemovedPlaceholder, *event.ResultPreview)
	}
}

func TestSanitizeValueRemovesEmbeddedBase64Runs(t *testing.T) {
	blob := strings.Repeat("YWJj", 180) // 720 chars
	sentence := "attached payload " + blob + " ends here"
	sanitized, ok := SanitizeValue(sentence).(string)
	if !ok {
		t.Fatalf("expected string, got %T", SanitizeValue(sentence))
	}
	if strings.Contains(sanitized, blob[:32]) {
		t.Fatalf("embedded base64 run survived: %s", sanitized)
	}
	if !strings.HasPrefix(sanitized, "attached payload ") || !strings.HasSuffix(sanitized, " ends here") {
		t.Fatalf("surrounding text mangled: %s", sanitized)
	}
	short := "short token QkJCQkJC stays"
	if got := SanitizeValue(short); got != short {
		t.Fatalf("short base64-alphabet token should be untouched: %v", got)
	}
}
