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
// Armature ingest endpoint on background goroutines by default; await delivery
// is available for serverless handlers. Close drains in-flight emissions.
package armatureanalytics

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// DeliveryMode controls whether event delivery runs off the request path or
// completes before RecordToolCall/RecordSessionInit return.
type DeliveryMode string

const (
	DeliveryBackground DeliveryMode = "background"
	DeliveryAwait      DeliveryMode = "await"
)

// Config configures the Recorder. APIKey is required unless Emit replaces
// network delivery. Other fields take package defaults when zero.
type Config struct {
	// APIKey authenticates with the Armature ingest endpoint. It is required
	// unless Emit replaces network delivery.
	APIKey string

	// EndpointURL overrides the ingest URL (default: DefaultEndpointURL).
	EndpointURL string

	// Timeout caps each ingest POST (default: DefaultTimeout).
	Timeout time.Duration

	// Delivery defaults to DeliveryBackground. Use DeliveryAwait in serverless
	// or short-lived handlers so telemetry is delivered before they freeze.
	Delivery DeliveryMode

	// Emit replaces network delivery, primarily for custom pipelines and tests.
	// When set, APIKey is optional.
	Emit func(context.Context, Batch) error

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

	// CaptureTelemetry is the master switch for conversation-derived telemetry
	// (user_intent, agent_thinking, user_frustration). nil or true
	// means on. When false the SDK injects no telemetry schema, appends no
	// description nudges (see InstrumentToolWithConfig), and never exports
	// telemetry values — including values sent by clients holding a cached
	// schema, which are stripped and dropped. Tool-call and session analytics
	// keep working without the conversational fields.
	CaptureTelemetry *bool

	// Redact, if set, runs over sanitized tool inputs/outputs (and the
	// normalized telemetry and error strings) before they are serialized into
	// event previews. It must return the value to serialize; a panic fails
	// closed — the affected payload is replaced with "[redaction failed]" and
	// the event still ships.
	Redact func(any) any

	// TelemetryFieldMap opts specific customer-owned argument fields into
	// export as Armature telemetry (TELEMETRY-CONTRACT.md). Keys are the V1
	// telemetry field names (user_intent, agent_thinking, user_frustration);
	// values are top-level argument property names to READ
	// (never strip) from the tool's arguments. Ignored while CaptureTelemetry
	// is false.
	TelemetryFieldMap map[string]string
}

func (c Config) captureEnabled() bool {
	return c.CaptureTelemetry == nil || *c.CaptureTelemetry
}

// Recorder owns the ingest client and the hook closures. Once registered on
// an mcp-go server via Hooks() or Install(), it tracks per-request timing and
// emits according to Config.Delivery.
type Recorder struct {
	cfg  Config
	send func(context.Context, Batch) error

	pendingCalls   sync.Map // requestKey → callContext
	sessionInfo    sync.Map // sessionID → *ClientInfo
	emittedSession *boundedKeySet
	lazySessions   *boundedKeySet

	inflight sync.WaitGroup
	// closeMu serializes the closed transition against emit's inflight.Add
	// so Close cannot finish draining between a completion's active() check
	// and its Add, which would let an event POST after Close returned.
	closeMu sync.Mutex

	dropped uint64
	closed  atomic.Bool
}

type callContext struct {
	toolName      string
	args          any
	telemetry     Telemetry
	startedAt     time.Time
	sessionID     string
	clientInfo    *ClientInfo
	actorSeed     string
	workflowRunID string
}

// NewRecorder constructs a Recorder. Returns ErrMissingAPIKey when both
// Config.APIKey and Config.Emit are empty (unless Config.Disabled is true, in
// which case the Recorder no-ops). Mirrors the TS SDK's recorder factory.
func NewRecorder(cfg Config) (*Recorder, error) {
	r := &Recorder{
		cfg:            cfg,
		emittedSession: newBoundedKeySet(10_000),
		lazySessions:   newBoundedKeySet(10_000),
	}
	if cfg.Disabled {
		return r, nil
	}
	if cfg.Emit != nil {
		r.send = cfg.Emit
		return r, nil
	}
	client, err := NewClient(cfg.APIKey, cfg.EndpointURL, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	r.send = client.Send
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
// in-flight POSTs via Flush. After Close returns, the recorder is unusable
// and no further ingest POSTs start.
func (r *Recorder) Close(ctx context.Context) error {
	r.closeMu.Lock()
	r.closed.Store(true)
	r.closeMu.Unlock()
	r.pendingCalls.Range(func(key, _ any) bool {
		r.pendingCalls.Delete(key)
		return true
	})
	return r.Flush(ctx)
}

// Dropped returns the count of events the recorder discarded because it was
// closed or disabled when the hook fired. Useful for sanity-checking
// shutdown behavior in tests.
func (r *Recorder) Dropped() uint64 {
	return atomic.LoadUint64(&r.dropped)
}

// RecordToolCall emits a framework-neutral tool-call event through this
// recorder. Adapter packages use this when their MCP framework does not expose
// mcp-go Hooks. ActorSeed falls back to Config.ActorSeed when it is empty.
func (r *Recorder) RecordToolCall(ctx context.Context, in ToolCallInput) {
	if !r.active() {
		return
	}
	if in.ActorSeed == "" {
		in.ActorSeed = r.ResolveActorSeed(ctx, nil)
	}
	// Single choke point for capture-off and field ownership
	// (TELEMETRY-CONTRACT.md): telemetry handed in by any path — the hooks,
	// adapter packages, direct callers, a cached-schema client — is dropped
	// here before the event is built, so it can never reach ingest or
	// OnError. A tool recorded as owning its telemetry field never exports
	// supplied telemetry either; the opt-in field map is the explicit way to
	// export customer fields, and it only applies while capture is on.
	if !r.cfg.captureEnabled() {
		in.Telemetry = Telemetry{}
	} else {
		if IsTelemetryOwnedTool(in.ToolName) {
			in.Telemetry = Telemetry{}
		}
		in.Telemetry = applyTelemetryFieldMap(in.Telemetry, in.Args, r.cfg.TelemetryFieldMap)
	}
	in.Redact = r.cfg.Redact
	if in.ClientInfo == nil {
		in.ClientInfo = ParseStatelessSessionClientInfo(in.SessionID)
	}
	events := make([]Event, 0, 2)
	if in.SessionID != "" && r.lazySessions.Add(ActorID(in.ActorSeed)+":"+in.SessionID) {
		events = append(events, BuildSessionInitEvent(SessionInitInput{
			SessionID:     in.SessionID,
			ActorSeed:     in.ActorSeed,
			StartedAt:     in.StartedAt,
			ClientInfo:    in.ClientInfo,
			WorkflowRunID: in.WorkflowRunID,
		}))
	}
	events = append(events, BuildToolCallEvent(in))
	r.emit(ctx, events)
}

// RecordSessionInit emits a framework-neutral session-init event through this
// recorder. Adapter packages use this when their MCP framework does not expose
// mcp-go Hooks. ActorSeed falls back to Config.ActorSeed when it is empty.
func (r *Recorder) RecordSessionInit(ctx context.Context, in SessionInitInput) {
	if !r.active() {
		return
	}
	if in.ActorSeed == "" {
		in.ActorSeed = r.ResolveActorSeed(ctx, nil)
	}
	if in.ClientInfo == nil {
		in.ClientInfo = ParseStatelessSessionClientInfo(in.SessionID)
	}
	if in.SessionID != "" && !r.lazySessions.Add(ActorID(in.ActorSeed)+":"+in.SessionID) {
		return
	}
	r.emit(ctx, []Event{BuildSessionInitEvent(in)})
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
	// Extract telemetry up-front so it survives into OnSuccess / OnError
	// regardless of whether the tool was registered via AddTool. Args used
	// for the input preview have telemetry stripped so the dashboards don't
	// double-show the same intent string. Tools that own their telemetry
	// field (TELEMETRY-CONTRACT.md, mode "owned") are exempt: their arguments
	// pass through untouched and nothing is interpreted as Armature telemetry.
	var telemetry Telemetry
	cleanedArgs := any(req.GetArguments())
	if !IsTelemetryOwnedTool(req.Params.Name) {
		telemetry, cleanedArgs = extractTelemetryFromArgs(req.GetArguments())
	}
	sessionKey := sessionKeyFromContext(ctx)
	key := callKey(sessionKey, id)
	registered := r.storePendingCall(key, callContext{
		toolName:      req.Params.Name,
		args:          cleanedArgs,
		telemetry:     telemetry,
		startedAt:     time.Now(),
		sessionID:     sessionIDFromContext(ctx),
		clientInfo:    r.clientInfoFor(sessionKey),
		actorSeed:     r.ResolveActorSeed(ctx, req.Header),
		workflowRunID: WorkflowRunIDFromHeaders(req.Header),
	})
	if !registered {
		return
	}
	context.AfterFunc(ctx, func() { r.onAbandonedCall(ctx, key) })
}

func (r *Recorder) onSuccess(ctx context.Context, id any, method mcp.MCPMethod, _ any, result any) {
	if method != mcp.MethodToolsCall {
		return
	}
	// Take the pending call before the active() check so a Close between
	// BeforeAny and here still cleans up the entry instead of leaking it.
	cc, ok := r.takeCall(sessionKeyFromContext(ctx), id)
	if !ok || !r.active() {
		return
	}

	isErr, _ := extractToolErrorFlag(result)
	r.RecordToolCall(ctx, ToolCallInput{
		ToolName:      cc.toolName,
		Args:          cc.args,
		Result:        result,
		IsToolError:   isErr,
		SessionID:     cc.sessionID,
		ActorSeed:     cc.actorSeed,
		StartedAt:     cc.startedAt,
		FinishedAt:    time.Now(),
		ClientInfo:    cc.clientInfo,
		Telemetry:     firstTelemetry(cc.telemetry, TelemetryFromContext(ctx)),
		WorkflowRunID: cc.workflowRunID,
	})
}

func (r *Recorder) onError(ctx context.Context, id any, method mcp.MCPMethod, _ any, callErr error) {
	if method != mcp.MethodToolsCall {
		return
	}
	// See onSuccess: clean up the pending call even when closed.
	cc, ok := r.takeCall(sessionKeyFromContext(ctx), id)
	if !ok || !r.active() {
		return
	}
	if callErr == nil {
		callErr = errors.New("tool_error")
	}
	r.RecordToolCall(ctx, ToolCallInput{
		ToolName:      cc.toolName,
		Args:          cc.args,
		Err:           callErr,
		SessionID:     cc.sessionID,
		ActorSeed:     cc.actorSeed,
		StartedAt:     cc.startedAt,
		FinishedAt:    time.Now(),
		ClientInfo:    cc.clientInfo,
		Telemetry:     firstTelemetry(cc.telemetry, TelemetryFromContext(ctx)),
		WorkflowRunID: cc.workflowRunID,
	})
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
	sessionKey := sessionKeyFromContext(ctx)
	info := &ClientInfo{
		Name:            message.Params.ClientInfo.Name,
		Version:         message.Params.ClientInfo.Version,
		ProtocolVersion: message.Params.ProtocolVersion,
	}
	if !isEmptySessionKey(sessionKey) {
		r.sessionInfo.Store(sessionKey, info)
	}

	if !r.emittedSession.Add(sessionKey) {
		return
	}

	r.RecordSessionInit(ctx, SessionInitInput{
		SessionID:     sessionIDFromContext(ctx),
		ActorSeed:     r.ResolveActorSeed(ctx, message.Header),
		StartedAt:     time.Now(),
		ClientInfo:    info,
		WorkflowRunID: WorkflowRunIDFromHeaders(message.Header),
	})
}

func (r *Recorder) onUnregisterSession(_ context.Context, session server.ClientSession) {
	if session == nil {
		return
	}
	key := sessionKeyForSession(session)
	r.sessionInfo.Delete(key)
	r.emittedSession.Delete(key)
}

// ---------- helpers ----------

func (r *Recorder) active() bool {
	if r == nil || r.cfg.Disabled || r.send == nil {
		return false
	}
	if r.closed.Load() {
		atomic.AddUint64(&r.dropped, 1)
		return false
	}
	return true
}

// ResolveActorSeed applies the configured resolver, then mirrors the
// TypeScript/Python default by falling back to the Authorization header.
func (r *Recorder) ResolveActorSeed(ctx context.Context, headers http.Header) string {
	if r.cfg.ActorSeed != nil {
		if seed := r.cfg.ActorSeed(ctx); seed != "" {
			return seed
		}
	}
	return headers.Get("Authorization")
}

func (r *Recorder) clientInfoFor(sessionKey any) *ClientInfo {
	if sessionKey == nil || isEmptySessionKey(sessionKey) {
		return nil
	}
	if v, ok := r.sessionInfo.Load(sessionKey); ok {
		return v.(*ClientInfo)
	}
	return nil
}

func (r *Recorder) takeCall(sessionKey, id any) (callContext, bool) {
	v, ok := r.pendingCalls.LoadAndDelete(callKey(sessionKey, id))
	if !ok {
		return callContext{}, false
	}
	return v.(callContext), true
}

// storePendingCall serializes registration with Close's closed transition.
// A call is therefore either visible to Close's sweep or rejected after the
// recorder is closed; it cannot be inserted into the gap between them.
func (r *Recorder) storePendingCall(key callKeyT, call callContext) bool {
	r.closeMu.Lock()
	defer r.closeMu.Unlock()
	if r.closed.Load() {
		atomic.AddUint64(&r.dropped, 1)
		return false
	}
	r.pendingCalls.Store(key, call)
	return true
}

func (r *Recorder) onAbandonedCall(ctx context.Context, key callKeyT) {
	value, ok := r.pendingCalls.LoadAndDelete(key)
	if !ok || !r.active() {
		return
	}
	cc := value.(callContext)
	callErr := context.Cause(ctx)
	if callErr == nil {
		callErr = context.Canceled
	}
	r.RecordToolCall(context.Background(), ToolCallInput{
		ToolName:      cc.toolName,
		Args:          cc.args,
		Err:           callErr,
		SessionID:     cc.sessionID,
		ActorSeed:     cc.actorSeed,
		StartedAt:     cc.startedAt,
		FinishedAt:    time.Now(),
		ClientInfo:    cc.clientInfo,
		Telemetry:     cc.telemetry,
		WorkflowRunID: cc.workflowRunID,
	})
}

// emit POSTs the events together, synchronously in await mode or on a tracked
// background goroutine otherwise, so Close/Flush can drain.
func (r *Recorder) emit(_ context.Context, events []Event) {
	if r.send == nil || len(events) == 0 {
		return
	}
	batch := Batch{SchemaVersion: SchemaVersion, Events: events}
	run := func() {
		emitCtx, cancel := context.WithTimeout(context.Background(), r.timeout())
		defer cancel()
		if err := r.send(emitCtx, batch); err != nil && r.cfg.OnError != nil {
			r.cfg.OnError(err, batch)
		}
	}
	// Register with inflight under closeMu: once Close flips closed, no new
	// POST can start, so Close's Flush covers everything that got in.
	r.closeMu.Lock()
	if r.closed.Load() {
		r.closeMu.Unlock()
		atomic.AddUint64(&r.dropped, 1)
		return
	}
	r.inflight.Add(1)
	r.closeMu.Unlock()
	if r.cfg.Delivery == DeliveryAwait {
		defer r.inflight.Done()
		run()
		return
	}
	// Detach from caller's context to avoid being cancelled by request
	// teardown; honour our own per-request timeout via the http.Client.
	go func() {
		defer r.inflight.Done()
		run()
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
		if id := s.SessionID(); id != "stdio" {
			return id
		}
		return ProcessScopedSessionID()
	}
	return ""
}

// sessionKeyFromContext returns a comparable per-connection key for pending
// calls, client-info tracking, and session_init dedup. It prefers the
// session id string; when the transport reports an empty id, the
// ClientSession value itself is used so concurrent sessionless connections
// do not collide on "".
func sessionKeyFromContext(ctx context.Context) any {
	if s := server.ClientSessionFromContext(ctx); s != nil {
		return sessionKeyForSession(s)
	}
	return ""
}

func sessionKeyForSession(s server.ClientSession) any {
	if sid := s.SessionID(); sid != "" {
		return sid
	}
	return s
}

// isEmptySessionKey reports whether key is the shared no-session fallback.
func isEmptySessionKey(key any) bool {
	s, ok := key.(string)
	return ok && s == ""
}

// callKey scopes a JSON-RPC request id by session so two concurrent sessions
// with colliding ids do not stomp each other's pending-call entries.
type callKeyT struct {
	sessionKey any
	id         any
}

func callKey(sessionKey, id any) callKeyT {
	return callKeyT{sessionKey: sessionKey, id: id}
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
