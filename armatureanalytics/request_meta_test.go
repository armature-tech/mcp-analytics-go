package armatureanalytics

import (
	"strings"
	"testing"
	"time"
)

func TestToolCallEventCarriesRequestMeta(t *testing.T) {
	event := BuildToolCallEvent(ToolCallInput{
		ToolName:   "echo",
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
		RequestMeta: map[string]any{
			"io.modelcontextprotocol/protocolVersion": "2026-07-28",
			"baggage": "gen_ai.conversation.id=conv-1",
		},
	})
	captured, ok := event.Metadata["request_meta"].(map[string]any)
	if !ok {
		t.Fatalf("request_meta = %#v, want verbatim map", event.Metadata["request_meta"])
	}
	if captured["io.modelcontextprotocol/protocolVersion"] != "2026-07-28" || captured["baggage"] != "gen_ai.conversation.id=conv-1" {
		t.Fatalf("request_meta content = %#v", captured)
	}
	if _, present := event.Metadata["request_meta_truncated"]; present {
		t.Fatal("small request_meta flagged truncated")
	}
}

func TestToolCallEventOmitsEmptyRequestMeta(t *testing.T) {
	event := BuildToolCallEvent(ToolCallInput{ToolName: "echo", StartedAt: time.Now(), FinishedAt: time.Now()})
	if _, present := event.Metadata["request_meta"]; present {
		t.Fatalf("empty request meta serialized: %#v", event.Metadata["request_meta"])
	}
}

func TestToolCallEventCapsOversizedRequestMeta(t *testing.T) {
	event := BuildToolCallEvent(ToolCallInput{
		ToolName:   "echo",
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
		RequestMeta: map[string]any{
			"huge": strings.Repeat("x", MaxRequestMetaBytes*2),
		},
	})
	preview, ok := event.Metadata["request_meta"].(string)
	if !ok {
		t.Fatalf("oversized request_meta = %T, want capped JSON string", event.Metadata["request_meta"])
	}
	if len(preview) > MaxRequestMetaBytes {
		t.Fatalf("capped request_meta is %d bytes, budget %d", len(preview), MaxRequestMetaBytes)
	}
	if event.Metadata["request_meta_truncated"] != true {
		t.Fatalf("missing truncation marker: %#v", event.Metadata)
	}
}
