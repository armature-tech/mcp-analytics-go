package armatureanalytics

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type redactionContextKey struct{}

func TestBuildToolCallEvent_Success(t *testing.T) {
	start := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	end := start.Add(150 * time.Millisecond)

	ev := BuildToolCallEvent(ToolCallInput{
		ToolName:   "list_items",
		Args:       map[string]any{"searchText": "cpu"},
		Result:     map[string]any{"metrics": []string{"a", "b"}},
		SessionID:  "sess-1",
		ActorSeed:  "user-1",
		StartedAt:  start,
		FinishedAt: end,
	})

	if ev.Kind != KindToolCall {
		t.Errorf("Kind = %q, want %q", ev.Kind, KindToolCall)
	}
	if !ev.OK {
		t.Errorf("OK = false, want true")
	}
	if ev.Error != nil {
		t.Errorf("Error = %v, want nil", *ev.Error)
	}
	if ev.DurationMs != 150 {
		t.Errorf("DurationMs = %d, want 150", ev.DurationMs)
	}
	if ev.SessionIDHint == nil || *ev.SessionIDHint != "sess-1" {
		t.Errorf("SessionIDHint = %v, want sess-1", ev.SessionIDHint)
	}
	if got := ev.Metadata["tool_name"]; got != "list_items" {
		t.Errorf("metadata.tool_name = %v", got)
	}
	if !strings.Contains(*ev.ScriptSource, "list_items") {
		t.Errorf("ScriptSource missing tool name: %q", *ev.ScriptSource)
	}
	if ev.ActorID == "" {
		t.Errorf("ActorID empty")
	}
	if ev.EventID == "" {
		t.Errorf("EventID empty")
	}
}

func TestBuildToolCallEvent_ErrorPaths(t *testing.T) {
	tests := []struct {
		name        string
		in          ToolCallInput
		wantOK      bool
		wantErrText string
	}{
		{
			name: "go error",
			in: ToolCallInput{
				ToolName:   "x",
				Err:        errors.New("upstream 500"),
				StartedAt:  time.Now(),
				FinishedAt: time.Now(),
			},
			wantOK:      false,
			wantErrText: "upstream 500",
		},
		{
			name: "tool error flag with classification",
			in: ToolCallInput{
				ToolName:    "x",
				IsToolError: true,
				ErrorType:   "auth_failed",
				StartedAt:   time.Now(),
				FinishedAt:  time.Now(),
			},
			wantOK:      false,
			wantErrText: "auth_failed",
		},
		{
			name: "tool error flag without classification",
			in: ToolCallInput{
				ToolName:    "x",
				IsToolError: true,
				StartedAt:   time.Now(),
				FinishedAt:  time.Now(),
			},
			wantOK:      false,
			wantErrText: "tool_error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := BuildToolCallEvent(tt.in)
			if ev.OK != tt.wantOK {
				t.Errorf("OK = %v, want %v", ev.OK, tt.wantOK)
			}
			if ev.Error == nil {
				t.Fatalf("Error = nil, want %q", tt.wantErrText)
			}
			if *ev.Error != tt.wantErrText {
				t.Errorf("Error = %q, want %q", *ev.Error, tt.wantErrText)
			}
		})
	}
}

func TestBuildToolCallEvent_AnonymousActor(t *testing.T) {
	a := BuildToolCallEvent(ToolCallInput{ToolName: "x", ActorSeed: "", StartedAt: time.Now(), FinishedAt: time.Now()}).ActorID
	b := BuildToolCallEvent(ToolCallInput{ToolName: "x", ActorSeed: "user-9", StartedAt: time.Now(), FinishedAt: time.Now()}).ActorID
	if a == b {
		t.Fatalf("anonymous and user-9 actor IDs should differ, both = %q", a)
	}
	if a != ActorID("") {
		t.Errorf("anonymous actor ID = %q, want %q", a, ActorID(""))
	}
}

func TestBuildSessionInitEvent_Shape(t *testing.T) {
	ev := BuildSessionInitEvent(SessionInitInput{
		SessionID: "sess-xyz",
		ActorSeed: "user-3",
		StartedAt: time.Now(),
		ClientInfo: &ClientInfo{
			Name:            "claude-code",
			Version:         "0.7",
			ProtocolVersion: "2025-06-18",
		},
	})
	if ev.Kind != KindSessionInit {
		t.Errorf("Kind = %q, want %q", ev.Kind, KindSessionInit)
	}
	if ev.DurationMs != 0 {
		t.Errorf("DurationMs = %d, want 0", ev.DurationMs)
	}
	if ev.SessionIDHint == nil || *ev.SessionIDHint != "sess-xyz" {
		t.Errorf("SessionIDHint = %v", ev.SessionIDHint)
	}
	if got := ev.Metadata["client_name"]; got != "claude-code" {
		t.Errorf("client_name = %v", got)
	}
	if got := ev.Metadata["protocol_version"]; got != "2025-06-18" {
		t.Errorf("protocol_version = %v", got)
	}
}

func TestTruncateUTF8(t *testing.T) {
	s := strings.Repeat("a", 100)
	got, trunc := truncateUTF8(s, 50)
	if !trunc {
		t.Fatalf("expected truncated=true")
	}
	if len(got) != 50 {
		t.Fatalf("len(got) = %d, want 50", len(got))
	}
}

func TestTruncateUTF8_RuneBoundary(t *testing.T) {
	// A 3-byte rune crossing the boundary should not produce an invalid
	// sequence — truncateUTF8 must back up to a rune start.
	s := strings.Repeat("é", 100) // é is 2 bytes in UTF-8
	got, trunc := truncateUTF8(s, 11)
	if !trunc {
		t.Fatalf("expected truncated=true")
	}
	// Should land on an even byte boundary (rune start).
	if len(got)%2 != 0 {
		t.Errorf("len(got) = %d, want even (rune boundary)", len(got))
	}
}

func TestStringifyPreview(t *testing.T) {
	if got := stringifyPreview(nil); got != "undefined" {
		t.Errorf("stringifyPreview(nil) = %q, want undefined", got)
	}
	if got := stringifyPreview(map[string]any{"k": 1}); got != `{"k":1}` {
		t.Errorf("stringifyPreview(map) = %q", got)
	}
	if got := stringifyPreview(make(chan int)); got != "[unserialisable]" {
		t.Errorf("stringifyPreview(chan) = %q, want [unserialisable]", got)
	}
}

func TestBuildToolCallEvent_DefaultSecretRedactionAndDisable(t *testing.T) {
	start := time.Unix(0, 0)
	secret := "AKIAIOSFODNN7EXAMPLE"
	in := ToolCallInput{
		ToolName: "private", Args: map[string]any{"password": "hunter2", "key": secret},
		Result:    map[string]any{"authorization": "Bearer abcdef1234567890abcdef"},
		Err:       errors.New("connect failed: password=hunter2"),
		Telemetry: Telemetry{UserIntent: "deploy with " + secret, AgentThinking: "password=hunter2"},
		StartedAt: start, FinishedAt: start,
	}
	event := BuildToolCallEvent(in)
	encoded, _ := json.Marshal(event)
	for _, raw := range []string{"hunter2", secret, "abcdef1234567890abcdef"} {
		if strings.Contains(string(encoded), raw) {
			t.Fatalf("raw secret %q leaked in event: %s", raw, encoded)
		}
	}
	disabled := false
	in.RedactSecrets = &disabled
	unredacted, _ := json.Marshal(BuildToolCallEvent(in))
	if !strings.Contains(string(unredacted), secret) {
		t.Fatalf("RedactSecrets=false did not preserve input: %s", unredacted)
	}
}

func TestFinalizeToolCallEventMutateDropAndFailClosed(t *testing.T) {
	start := time.Unix(0, 0)
	ctx := context.WithValue(context.Background(), redactionContextKey{}, "visible")
	base := ToolCallInput{
		ToolName: "original", Args: map[string]any{"password": "hunter2"},
		Result: map[string]any{"secret": "value"}, Err: errors.New("secret=hunter2"),
		Telemetry: Telemetry{UserIntent: "password=hunter2"},
		StartedAt: start, FinishedAt: start.Add(time.Millisecond),
	}
	mutated := FinalizeToolCallEvent(ctx, base, func(hookCtx context.Context, candidate *RedactableToolCall) (*RedactableToolCall, error) {
		if hookCtx.Value(redactionContextKey{}) != "visible" {
			t.Fatal("hook did not receive recorder context")
		}
		candidate.ToolName = "mutated"
		candidate.Input = map[string]any{"safe": true}
		return candidate, nil
	})
	if mutated == nil || mutated.Metadata["tool_name"] != "mutated" || mutated.Metadata["input_preview"] != `{"safe":true}` {
		t.Fatalf("mutated event = %#v", mutated)
	}
	if dropped := FinalizeToolCallEvent(ctx, base, func(context.Context, *RedactableToolCall) (*RedactableToolCall, error) {
		return nil, nil
	}); dropped != nil {
		t.Fatalf("drop hook returned event: %#v", dropped)
	}
	failed := FinalizeToolCallEvent(ctx, base, func(context.Context, *RedactableToolCall) (*RedactableToolCall, error) {
		return nil, errors.New("nope")
	})
	failedJSON, _ := json.Marshal(failed)
	if strings.Contains(string(failedJSON), "hunter2") || strings.Contains(string(failedJSON), "value") {
		t.Fatalf("hook failure leaked raw candidate: %s", failedJSON)
	}
}

func TestBuildToolCallEvent_RequestIDScopedBySession(t *testing.T) {
	// Shape-C hazard (#1402): a caller-supplied RequestID (e.g. a per-connection
	// JSON-RPC counter reused across concurrent conversations) must not collide
	// on event_id across sessions, but a genuine within-session retry must.
	start := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Millisecond)
	build := func(session string) Event {
		return BuildToolCallEvent(ToolCallInput{
			ToolName: "ping", RequestID: "5", SessionID: session,
			ActorSeed: "anonymous", StartedAt: start, FinishedAt: end,
		})
	}
	a1 := build("sess-A")
	b := build("sess-B")
	a2 := build("sess-A")
	if a1.EventID == b.EventID {
		t.Errorf("same RequestID across sessions collided: %s", a1.EventID)
	}
	if a1.EventID != a2.EventID {
		t.Errorf("same RequestID within a session must de-dup: %s vs %s", a1.EventID, a2.EventID)
	}

	// Unscoped when no session is known; minted (unique) when RequestID absent.
	noSess := BuildToolCallEvent(ToolCallInput{ToolName: "ping", RequestID: "5", ActorSeed: "anonymous", StartedAt: start, FinishedAt: end})
	if noSess.EventID != EventID(ActorID("anonymous"), KindToolCall, "5") {
		t.Errorf("caller RequestID must pass through verbatim when no session is known")
	}
	m1 := BuildToolCallEvent(ToolCallInput{ToolName: "ping", ActorSeed: "anonymous", StartedAt: start, FinishedAt: end})
	m2 := BuildToolCallEvent(ToolCallInput{ToolName: "ping", ActorSeed: "anonymous", StartedAt: start, FinishedAt: end})
	if m1.EventID == m2.EventID {
		t.Errorf("absent RequestID must mint distinct ids")
	}
}
