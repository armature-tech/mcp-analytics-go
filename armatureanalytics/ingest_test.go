package armatureanalytics_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/armature-tech/mcp-analytics-go/armatureanalytics"
)

type recordingServer struct {
	t      *testing.T
	server *httptest.Server

	mu      sync.Mutex
	bodies  [][]byte
	auth    string
	ua      string
	respond int
	delay   time.Duration
}

func newRecordingServer(t *testing.T) *recordingServer {
	rs := &recordingServer{t: t, respond: http.StatusAccepted}
	rs.server = httptest.NewServer(http.HandlerFunc(rs.handle))
	t.Cleanup(rs.server.Close)
	return rs
}

func (rs *recordingServer) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		rs.t.Fatalf("read body: %v", err)
	}
	rs.mu.Lock()
	rs.bodies = append(rs.bodies, body)
	rs.auth = r.Header.Get("Authorization")
	rs.ua = r.Header.Get("User-Agent")
	delay := rs.delay
	respond := rs.respond
	rs.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	w.WriteHeader(respond)
}

func (rs *recordingServer) Bodies() [][]byte {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([][]byte, len(rs.bodies))
	copy(out, rs.bodies)
	return out
}

func (rs *recordingServer) Auth() string { rs.mu.Lock(); defer rs.mu.Unlock(); return rs.auth }
func (rs *recordingServer) UA() string   { rs.mu.Lock(); defer rs.mu.Unlock(); return rs.ua }

func TestNewClient_RequiresAPIKey(t *testing.T) {
	if _, err := armatureanalytics.NewClient("", "", 0); err == nil {
		t.Fatalf("expected ErrMissingAPIKey")
	}
}

func TestNewClient_AppliesDefaults(t *testing.T) {
	c, err := armatureanalytics.NewClient("k", "", 0)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c == nil {
		t.Fatalf("nil client")
	}
}

func TestClient_SendsBatch(t *testing.T) {
	rs := newRecordingServer(t)
	c, err := armatureanalytics.NewClient("test-key", rs.server.URL, 2*time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ev := armatureanalytics.BuildToolCallEvent(armatureanalytics.ToolCallInput{
		ToolName:   "do_thing",
		ActorSeed:  "user-1",
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
	})
	if err := c.SendEvent(context.Background(), ev); err != nil {
		t.Fatalf("SendEvent: %v", err)
	}

	bodies := rs.Bodies()
	if len(bodies) != 1 {
		t.Fatalf("got %d batches, want 1", len(bodies))
	}
	if got := rs.Auth(); got != "Bearer test-key" {
		t.Errorf("Authorization = %q", got)
	}
	if got := rs.UA(); got == "" {
		t.Errorf("User-Agent missing")
	}

	var decoded map[string]any
	if err := json.Unmarshal(bodies[0], &decoded); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if decoded["schema_version"].(float64) != 1 {
		t.Errorf("schema_version = %v, want 1", decoded["schema_version"])
	}
	events := decoded["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("got %d events", len(events))
	}
	e := events[0].(map[string]any)
	if e["kind"] != "tool_call" {
		t.Errorf("kind = %v", e["kind"])
	}
}

func TestClient_Non2xxIsError(t *testing.T) {
	rs := newRecordingServer(t)
	rs.respond = http.StatusUnauthorized
	c, _ := armatureanalytics.NewClient("k", rs.server.URL, time.Second)

	err := c.SendEvent(context.Background(), armatureanalytics.Event{Kind: "tool_call"})
	if err == nil {
		t.Fatalf("expected error for 401")
	}
	var deliveryErr *armatureanalytics.DeliveryError
	if !errors.As(err, &deliveryErr) || deliveryErr.Status != 401 || deliveryErr.Attempts != 1 || deliveryErr.Retryable {
		t.Fatalf("unexpected delivery error: %#v", err)
	}
}

func TestClient_RetriesTransientFailureOnce(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	c, _ := armatureanalytics.NewClient("k", srv.URL, time.Second)
	if err := c.SendEvent(context.Background(), armatureanalytics.Event{Kind: "tool_call"}); err != nil {
		t.Fatalf("SendEvent: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestClient_PreservesServerDiagnosticWithoutResponseDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"ingest_key_wrong_region","message":"secret-key"}}`))
	}))
	defer srv.Close()
	c, _ := armatureanalytics.NewClient("k", srv.URL, time.Second)
	err := c.SendEvent(context.Background(), armatureanalytics.Event{Kind: "tool_call"})
	var deliveryErr *armatureanalytics.DeliveryError
	if !errors.As(err, &deliveryErr) || deliveryErr.Code != "ingest_key_wrong_region" {
		t.Fatalf("unexpected delivery error: %#v", err)
	}
	if got := err.Error(); got == "" || strings.Contains(got, "secret-key") {
		t.Fatalf("unsafe delivery error: %q", got)
	}
}

func TestClient_Timeout(t *testing.T) {
	rs := newRecordingServer(t)
	rs.delay = 200 * time.Millisecond
	c, _ := armatureanalytics.NewClient("k", rs.server.URL, 20*time.Millisecond)

	err := c.SendEvent(context.Background(), armatureanalytics.Event{Kind: "tool_call"})
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	var deliveryErr *armatureanalytics.DeliveryError
	if !errors.As(err, &deliveryErr) || deliveryErr.Code != "ingest_timeout" || deliveryErr.Attempts != 2 {
		t.Fatalf("unexpected delivery error: %#v", err)
	}
	if calls := len(rs.Bodies()); calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}
