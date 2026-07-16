package armatureanalytics

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestRecorderAwaitCustomEmitterAndLazySessionInit(t *testing.T) {
	var batches []Batch
	recorder, err := NewRecorder(Config{
		Delivery: DeliveryAwait,
		Emit: func(_ context.Context, batch Batch) error {
			batches = append(batches, batch)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := BuildStatelessSessionID(&ClientInfo{Name: "claude-code", Version: "2.0.13"})
	now := time.Now()
	recorder.RecordToolCall(context.Background(), ToolCallInput{
		ToolName:      "lookup",
		Args:          map[string]any{},
		SessionID:     sessionID,
		StartedAt:     now,
		FinishedAt:    now.Add(time.Millisecond),
		WorkflowRunID: "019f6a11-7064-7e32-8b6f-6aef04cc6414",
	})
	if len(batches) != 1 || len(batches[0].Events) != 2 {
		t.Fatalf("batches = %#v, want one session_init + tool_call batch", batches)
	}
	sessionEvent, toolEvent := batches[0].Events[0], batches[0].Events[1]
	if sessionEvent.Kind != KindSessionInit || toolEvent.Kind != KindToolCall {
		t.Fatalf("event kinds = %q, %q", sessionEvent.Kind, toolEvent.Kind)
	}
	if sessionEvent.Metadata["client_name"] != "claude-code" || toolEvent.Metadata["client_name"] != "claude-code" {
		t.Fatalf("stateless client identity was not recovered: %#v / %#v", sessionEvent.Metadata, toolEvent.Metadata)
	}
	if !sessionEvent.IsWorkflow || !toolEvent.IsWorkflow || toolEvent.WorkflowRunID == "" {
		t.Fatalf("workflow markers missing: %#v / %#v", sessionEvent, toolEvent)
	}

	recorder.RecordToolCall(context.Background(), ToolCallInput{
		ToolName:   "lookup",
		SessionID:  sessionID,
		StartedAt:  now,
		FinishedAt: now.Add(time.Millisecond),
	})
	if len(batches) != 2 || len(batches[1].Events) != 1 || batches[1].Events[0].Kind != KindToolCall {
		t.Fatalf("second call should not duplicate session_init: %#v", batches)
	}
	recorder.RecordSessionInit(context.Background(), SessionInitInput{
		SessionID: sessionID,
		StartedAt: now,
	})
	if len(batches) != 2 {
		t.Fatalf("explicit session init should share the bounded dedupe set: %#v", batches)
	}
}

func TestRequestParityHelpers(t *testing.T) {
	recorder, err := NewRecorder(Config{Emit: func(context.Context, Batch) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{
		"Authorization":     []string{"Bearer stable-principal"},
		WorkflowRunIDHeader: []string{"019f6a11-7064-7e32-8b6f-6aef04cc6414"},
	}
	if got := recorder.ResolveActorSeed(context.Background(), headers); got != "Bearer stable-principal" {
		t.Fatalf("actor seed = %q", got)
	}
	if got := WorkflowRunIDFromHeaders(headers); got != "019f6a11-7064-7e32-8b6f-6aef04cc6414" {
		t.Fatalf("workflow run id = %q", got)
	}
	headers.Set(WorkflowRunIDHeader, "not-a-uuid")
	if got := WorkflowRunIDFromHeaders(headers); got != "" {
		t.Fatalf("invalid workflow id accepted: %q", got)
	}
}

func TestBoundedSessionKeysEvictAndDelete(t *testing.T) {
	keys := newBoundedKeySet(2)
	first := &struct{ name string }{"first"}
	second := &struct{ name string }{"second"}
	third := &struct{ name string }{"third"}
	if !keys.Add(first) || !keys.Add(second) || keys.Add(first) {
		t.Fatal("initial key insertion or deduplication failed")
	}
	if !keys.Add(third) {
		t.Fatal("third key was not inserted")
	}
	if !keys.Add(first) {
		t.Fatal("oldest key was not evicted")
	}
	keys.Delete(first)
	if !keys.Add(first) {
		t.Fatal("deleted key was not forgotten")
	}
}
