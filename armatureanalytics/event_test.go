package armatureanalytics

import (
	"errors"
	"strings"
	"testing"
	"time"
)

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
