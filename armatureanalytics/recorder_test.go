package armatureanalytics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// recordingSink captures ingest batches posted by the recorder.
type recordingSink struct {
	mu     sync.Mutex
	events []map[string]any
	srv    *httptest.Server
}

func newRecordingSink(t *testing.T) *recordingSink {
	s := &recordingSink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var batch struct {
			Events []map[string]any `json:"events"`
		}
		_ = json.Unmarshal(body, &batch)
		s.mu.Lock()
		s.events = append(s.events, batch.Events...)
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *recordingSink) Events() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, len(s.events))
	copy(out, s.events)
	return out
}

// fakeSession is a ClientSession with a fixed (possibly empty) session id,
// standing in for transports that do not mint session ids.
type fakeSession struct {
	id     string
	ch     chan mcp.JSONRPCNotification
	inited bool
}

func newFakeSession(id string) *fakeSession {
	return &fakeSession{id: id, ch: make(chan mcp.JSONRPCNotification, 8)}
}

func (f *fakeSession) Initialize()       { f.inited = true }
func (f *fakeSession) Initialized() bool { return f.inited }
func (f *fakeSession) NotificationChannel() chan<- mcp.JSONRPCNotification {
	return f.ch
}
func (f *fakeSession) SessionID() string { return f.id }

func sessionCtx(sess server.ClientSession) context.Context {
	srv := server.NewMCPServer("recorder-test", "0")
	return srv.WithContext(context.Background(), sess)
}

func newSinkRecorder(t *testing.T, sink *recordingSink) *Recorder {
	t.Helper()
	rec, err := NewRecorder(Config{APIKey: "test-key", EndpointURL: sink.srv.URL})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close(context.Background()) })
	return rec
}

func TestSessionInit_EmptySessionID_DedupedPerConnection(t *testing.T) {
	sink := newRecordingSink(t)
	rec := newSinkRecorder(t, sink)

	connA := sessionCtx(newFakeSession(""))
	connB := sessionCtx(newFakeSession(""))
	initMsg := &mcp.InitializeRequest{}

	rec.onAfterInitialize(connA, 1, initMsg, nil)
	rec.onAfterInitialize(connA, 2, initMsg, nil) // re-initialize on the same connection
	rec.onAfterInitialize(connB, 1, initMsg, nil) // a different sessionless connection

	if err := rec.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := len(sink.Events()); got != 2 {
		t.Fatalf("session_init events = %d, want 2 (deduped per connection, not per empty id)", got)
	}
}

func TestPendingCalls_EmptySessionID_NoCrossConnectionCollision(t *testing.T) {
	sink := newRecordingSink(t)
	rec := newSinkRecorder(t, sink)

	connA := sessionCtx(newFakeSession(""))
	connB := sessionCtx(newFakeSession(""))

	reqA := &mcp.CallToolRequest{}
	reqA.Params.Name = "tool_a"
	reqB := &mcp.CallToolRequest{}
	reqB.Params.Name = "tool_b"

	// The same JSON-RPC id on two concurrent sessionless connections must not
	// stomp each other's pending-call state.
	rec.onBeforeAny(connA, int64(1), mcp.MethodToolsCall, reqA)
	rec.onBeforeAny(connB, int64(1), mcp.MethodToolsCall, reqB)
	rec.onSuccess(connA, int64(1), mcp.MethodToolsCall, reqA, &mcp.CallToolResult{})
	rec.onSuccess(connB, int64(1), mcp.MethodToolsCall, reqB, &mcp.CallToolResult{})

	if err := rec.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	events := sink.Events()
	if len(events) != 2 {
		t.Fatalf("tool_call events = %d, want 2; events=%v", len(events), events)
	}
	names := map[string]bool{}
	for _, ev := range events {
		meta, _ := ev["metadata"].(map[string]any)
		names[fmt.Sprint(meta["tool_name"])] = true
	}
	if !names["tool_a"] || !names["tool_b"] {
		t.Fatalf("expected one event per tool, got %v", names)
	}
}

func TestSessionlessConnection_ToolCallCarriesClientInfo(t *testing.T) {
	sink := newRecordingSink(t)
	rec := newSinkRecorder(t, sink)

	conn := sessionCtx(newFakeSession(""))
	initMsg := &mcp.InitializeRequest{}
	initMsg.Params.ClientInfo.Name = "fake-client"
	initMsg.Params.ClientInfo.Version = "9.9"
	initMsg.Params.ProtocolVersion = "2025-06-18"
	rec.onAfterInitialize(conn, 1, initMsg, nil)

	req := &mcp.CallToolRequest{}
	req.Params.Name = "ping"
	rec.onBeforeAny(conn, int64(2), mcp.MethodToolsCall, req)
	rec.onSuccess(conn, int64(2), mcp.MethodToolsCall, req, &mcp.CallToolResult{})

	if err := rec.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	var found bool
	for _, ev := range sink.Events() {
		if ev["kind"] != "tool_call" {
			continue
		}
		found = true
		meta, _ := ev["metadata"].(map[string]any)
		if meta["client_name"] != "fake-client" {
			t.Errorf("tool_call on sessionless transport lost client info: %v", meta)
		}
	}
	if !found {
		t.Fatalf("no tool_call event captured; events=%v", sink.Events())
	}
}

func TestToolCallCompletion_AfterClose_ClearsPendingCall(t *testing.T) {
	sink := newRecordingSink(t)
	rec := newSinkRecorder(t, sink)

	conn := sessionCtx(newFakeSession("s-1"))
	req := &mcp.CallToolRequest{}
	req.Params.Name = "slow"

	rec.onBeforeAny(conn, int64(7), mcp.MethodToolsCall, req)
	if err := rec.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rec.onSuccess(conn, int64(7), mcp.MethodToolsCall, req, &mcp.CallToolResult{})

	pending := 0
	rec.pendingCalls.Range(func(_, _ any) bool { pending++; return true })
	if pending != 0 {
		t.Fatalf("pendingCalls entries after Close+completion = %d, want 0", pending)
	}
	if got := len(sink.Events()); got != 0 {
		t.Fatalf("closed recorder emitted %d events, want 0", got)
	}
}
