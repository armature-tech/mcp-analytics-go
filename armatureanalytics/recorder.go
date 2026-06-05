// Package armatureanalytics is a drop-in observability layer for MCP servers
// built on github.com/mark3labs/mcp-go.
//
// Usage:
//
//	rec, err := armatureanalytics.NewRecorder(armatureanalytics.Config{
//	    APIKey: os.Getenv("ANALYTICS_INGEST_API_KEY"),
//	})
//	if err != nil { /* handle */ }
//	defer rec.Close(context.Background())
//
//	s := server.NewMCPServer("my-mcp", "1.0", server.WithHooks(rec.Hooks()))
//	// register tools as usual...
//
// The recorder captures one tool_call event per MCP tool invocation
// (BeforeAny + OnSuccess/OnError filtered to "tools/call") and one
// session_init event per session (AfterInitialize). Events are POSTed to the
// Armature ingest endpoint on background goroutines tracked by an internal
// WaitGroup; Close drains in-flight emissions before returning.
package armatureanalytics

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Config configures the Recorder. APIKey is required. Other fields take the
// package defaults when zero.
type Config struct {
	// APIKey authenticates with the Armature ingest endpoint. Required.
	APIKey string

	// EndpointURL overrides the ingest URL (default: DefaultEndpointURL).
	EndpointURL string

	// Timeout caps each ingest POST (default: DefaultTimeout).
	Timeout time.Duration

	// ActorSeed returns the actor seed (typically the auth principal) for a
	// given request context. The seed is sha256-hashed into the actor_id on
	// the wire. If nil or it returns "", the actor is recorded as
	// sha256("anonymous").
	ActorSeed func(ctx context.Context) string

	// OnError, if set, receives any ingest delivery failures. When nil,
	// failures are silently dropped — call sites should set this in
	// production to surface delivery problems through their own logger.
	OnError func(err error, batch Batch)

	// Disabled, when true, causes all hooks to no-op without contacting the
	// ingest endpoint. Useful for opt-in via env var without restructuring
	// the call site.
	Disabled bool
}

// Recorder owns the ingest client and the hook closures. Once registered on
// an mcp-go server via Hooks() or Install(), it tracks per-request timing
// and emits events asynchronously.
type Recorder struct {
	cfg    Config
	client *Client

	pendingCalls   sync.Map // requestKey → callContext
	sessionInfo    sync.Map // sessionID → *ClientInfo
	emittedSession sync.Map // sessionID → struct{}; dedup session_init

	inflight sync.WaitGroup

	dropped uint64
	closed  atomic.Bool
}

type callContext struct {
	toolName  string
	args      any
	telemetry Telemetry
	startedAt time.Time
	sessionID string
	actorSeed string
}

// NewRecorder constructs a Recorder. Returns ErrMissingAPIKey when
// Config.APIKey is empty (unless Config.Disabled is true, in which case the
// Recorder no-ops). Mirrors the TS SDK's createAnalyticsRecorder.
func NewRecorder(cfg Config) (*Recorder, error) {
	r := &Recorder{cfg: cfg}
	if cfg.Disabled {
		return r, nil
	}
	client, err := NewClient(cfg.APIKey, cfg.EndpointURL, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	r.client = client
	return r, nil
}

// Hooks returns a fresh *server.Hooks with the recorder's tool-call and
// session hooks pre-registered. Pass it to server.WithHooks at MCPServer
// construction.
func (r *Recorder) Hooks() *server.Hooks {
	h := &server.Hooks{}
	r.Install(h)
	return h
}

// Install adds the recorder's hooks to an existing *server.Hooks. Use this
// when the caller maintains their own hooks bundle (e.g. for tracing or
// structured logging). Safe to call once per Hooks instance.
func (r *Recorder) Install(h *server.Hooks) {
	if r == nil || h == nil {
		return
	}
	h.AddBeforeAny(r.onBeforeAny)
	h.AddOnSuccess(r.onSuccess)
	h.AddOnError(r.onError)
	h.AddAfterInitialize(r.onAfterInitialize)
	h.AddOnUnregisterSession(r.onUnregisterSession)
}

// Flush blocks until all in-flight ingest POSTs complete or ctx is cancelled.
// Returns ctx.Err() on cancellation; nil otherwise.
func (r *Recorder) Flush(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		r.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close marks the recorder closed (so further events drop) and waits for
// in-flight POSTs via Flush. After Close returns, the recorder is unusable.
func (r *Recorder) Close(ctx context.Context) error {
	r.closed.Store(true)
	return r.Flush(ctx)
}

// Dropped returns the count of events the recorder discarded because it was
// closed or disabled when the hook fired. Useful for sanity-checking
// shutdown behavior in tests.
func (r *Recorder) Dropped() uint64 {
	return atomic.LoadUint64(&r.dropped)
}

// ---------- hooks ----------

func (r *Recorder) onBeforeAny(ctx context.Context, id any, method mcp.MCPMethod, message any) {
	if !r.active() {
		return
	}
	if method != mcp.MethodToolsCall {
		return
	}
	req, ok := message.(*mcp.CallToolRequest)
	if !ok || req == nil {
		return
	}
	sessionID := sessionIDFromContext(ctx)
	// Extract telemetry up-front so it survives into OnSuccess / OnError
	// regardless of whether the tool was registered via AddTool. Args used
	// for the input preview have telemetry stripped so the dashboards don't
	// double-show the same intent string.
	telemetry, cleanedArgs := extractTelemetryFromArgs(req.GetArguments())
	r.pendingCalls.Store(callKey(sessionID, id), callContext{
		toolName:  req.Params.Name,
		args:      cleanedArgs,
		telemetry: telemetry,
		startedAt: time.Now(),
		sessionID: sessionID,
		actorSeed: r.actorSeed(ctx),
	})
}

func (r *Recorder) onSuccess(ctx context.Context, id any, method mcp.MCPMethod, _ any, result any) {
	if !r.active() || method != mcp.MethodToolsCall {
		return
	}
	cc, ok := r.takeCall(sessionIDFromContext(ctx), id)
	if !ok {
		return
	}

	isErr, _ := extractToolErrorFlag(result)
	r.emit(ctx, BuildToolCallEvent(ToolCallInput{
		ToolName:    cc.toolName,
		Args:        cc.args,
		Result:      result,
		IsToolError: isErr,
		SessionID:   cc.sessionID,
		ActorSeed:   cc.actorSeed,
		StartedAt:   cc.startedAt,
		FinishedAt:  time.Now(),
		ClientInfo:  r.clientInfoFor(cc.sessionID),
		Telemetry:   firstTelemetry(cc.telemetry, TelemetryFromContext(ctx)),
	}))
}

func (r *Recorder) onError(ctx context.Context, id any, method mcp.MCPMethod, _ any, callErr error) {
	if !r.active() || method != mcp.MethodToolsCall {
		return
	}
	cc, ok := r.takeCall(sessionIDFromContext(ctx), id)
	if !ok {
		return
	}
	if callErr == nil {
		callErr = errors.New("tool_error")
	}
	r.emit(ctx, BuildToolCallEvent(ToolCallInput{
		ToolName:   cc.toolName,
		Args:       cc.args,
		Err:        callErr,
		SessionID:  cc.sessionID,
		ActorSeed:  cc.actorSeed,
		StartedAt:  cc.startedAt,
		FinishedAt: time.Now(),
		ClientInfo: r.clientInfoFor(cc.sessionID),
		Telemetry:  firstTelemetry(cc.telemetry, TelemetryFromContext(ctx)),
	}))
}

// firstTelemetry returns a if any field is set, else b. Used to prefer the
// telemetry captured in BeforeAny (off the raw args) over the one stashed by
// AddTool's handler wrap, since both come from the same args block.
func firstTelemetry(a, b Telemetry) Telemetry {
	if a != (Telemetry{}) {
		return a
	}
	return b
}

func (r *Recorder) onAfterInitialize(ctx context.Context, _ any, message *mcp.InitializeRequest, _ *mcp.InitializeResult) {
	if !r.active() || message == nil {
		return
	}
	sessionID := sessionIDFromContext(ctx)
	info := &ClientInfo{
		Name:            message.Params.ClientInfo.Name,
		Version:         message.Params.ClientInfo.Version,
		ProtocolVersion: message.Params.ProtocolVersion,
	}
	if sessionID != "" {
		r.sessionInfo.Store(sessionID, info)
	}

	if _, already := r.emittedSession.LoadOrStore(sessionID, struct{}{}); already && sessionID != "" {
		return
	}

	r.emit(ctx, BuildSessionInitEvent(SessionInitInput{
		SessionID:  sessionID,
		ActorSeed:  r.actorSeed(ctx),
		StartedAt:  time.Now(),
		ClientInfo: info,
	}))
}

func (r *Recorder) onUnregisterSession(_ context.Context, session server.ClientSession) {
	if session == nil {
		return
	}
	sid := session.SessionID()
	r.sessionInfo.Delete(sid)
	r.emittedSession.Delete(sid)
}

// ---------- helpers ----------

func (r *Recorder) active() bool {
	if r == nil || r.cfg.Disabled || r.client == nil {
		return false
	}
	if r.closed.Load() {
		atomic.AddUint64(&r.dropped, 1)
		return false
	}
	return true
}

func (r *Recorder) actorSeed(ctx context.Context) string {
	if r.cfg.ActorSeed == nil {
		return ""
	}
	return r.cfg.ActorSeed(ctx)
}

func (r *Recorder) clientInfoFor(sessionID string) *ClientInfo {
	if sessionID == "" {
		return nil
	}
	if v, ok := r.sessionInfo.Load(sessionID); ok {
		return v.(*ClientInfo)
	}
	return nil
}

func (r *Recorder) takeCall(sessionID string, id any) (callContext, bool) {
	v, ok := r.pendingCalls.LoadAndDelete(callKey(sessionID, id))
	if !ok {
		return callContext{}, false
	}
	return v.(callContext), true
}

// emit POSTs the event on a background goroutine tracked by inflight so
// Close/Flush can drain.
func (r *Recorder) emit(ctx context.Context, ev Event) {
	if r.client == nil {
		return
	}
	r.inflight.Add(1)
	// Detach from caller's context to avoid being cancelled by request
	// teardown; honour our own per-request timeout via the http.Client.
	go func() {
		defer r.inflight.Done()
		emitCtx, cancel := context.WithTimeout(context.Background(), r.timeout())
		defer cancel()
		batch := Batch{SchemaVersion: SchemaVersion, Events: []Event{ev}}
		if err := r.client.Send(emitCtx, batch); err != nil {
			if r.cfg.OnError != nil {
				r.cfg.OnError(err, batch)
			}
		}
	}()
}

func (r *Recorder) timeout() time.Duration {
	if r.cfg.Timeout > 0 {
		return r.cfg.Timeout
	}
	return DefaultTimeout
}

func sessionIDFromContext(ctx context.Context) string {
	if s := server.ClientSessionFromContext(ctx); s != nil {
		return s.SessionID()
	}
	return ""
}

// callKey scopes a JSON-RPC request id by session so two concurrent sessions
// with colliding ids do not stomp each other's pending-call entries.
type callKeyT struct {
	sessionID string
	id        any
}

func callKey(sessionID string, id any) callKeyT {
	return callKeyT{sessionID: sessionID, id: id}
}

// extractToolErrorFlag pulls IsError off an mcp.CallToolResult-shaped value.
// Returns (false, false) when the result is not the expected type, which the
// caller treats as a success.
func extractToolErrorFlag(result any) (isErr bool, hasFlag bool) {
	if r, ok := result.(*mcp.CallToolResult); ok && r != nil {
		return r.IsError, true
	}
	if r, ok := result.(mcp.CallToolResult); ok {
		return r.IsError, true
	}
	return false, false
}
