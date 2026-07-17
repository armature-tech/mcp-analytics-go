// Package official integrates Armature analytics with the official
// github.com/modelcontextprotocol/go-sdk/mcp server implementation.
package official

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	armatureanalytics "github.com/armature-tech/mcp-analytics-go/armatureanalytics"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	methodInitialize = "initialize"
	methodToolsCall  = "tools/call"
)

// serverTelemetryConfigs lets InstrumentTool inherit the capture policy from
// NewMCPServerWithConfig without changing its established call signature.
// Entries are removed by the paired Shutdown function.
var serverTelemetryConfigs sync.Map // *mcp.Server -> Config

// Config configures analytics delivery. It is shared with the mark3labs
// adapter so both integrations emit the same wire format.
type Config = armatureanalytics.Config

// Batch is the analytics ingest envelope passed to Config.OnError.
type Batch = armatureanalytics.Batch

// Shutdown drains in-flight analytics deliveries.
type Shutdown = armatureanalytics.Shutdown

type sessionInfo struct {
	clientInfo         *armatureanalytics.ClientInfo
	analyticsSessionID string
	emitted            bool
	session            *mcp.ServerSession
}

// Recorder installs official-SDK middleware and owns analytics delivery.
type Recorder struct {
	core *armatureanalytics.Recorder

	installMu sync.Mutex
	installed map[*mcp.Server]struct{}

	sessionsMu sync.Mutex
	sessions   map[any]sessionInfo
}

// EnvConfig reads ANALYTICS_INGEST_API_KEY and ANALYTICS_INGEST_URL.
func EnvConfig() Config {
	return armatureanalytics.EnvConfig()
}

// NewRecorder constructs an official-SDK recorder. An empty API key returns
// armatureanalytics.ErrMissingAPIKey unless cfg.Disabled is true or cfg.Emit
// replaces network delivery.
func NewRecorder(cfg Config) (*Recorder, error) {
	core, err := armatureanalytics.NewRecorder(cfg)
	if err != nil {
		return nil, err
	}
	return &Recorder{
		core:      core,
		installed: make(map[*mcp.Server]struct{}),
		sessions:  make(map[any]sessionInfo),
	}, nil
}

// NewMCPServer constructs an official mcp.Server with analytics middleware
// configured from the environment. A missing API key cleanly disables
// analytics and makes the returned Shutdown a no-op.
func NewMCPServer(impl *mcp.Implementation, opts *mcp.ServerOptions) (*mcp.Server, Shutdown) {
	return NewMCPServerWithConfig(impl, opts, EnvConfig())
}

// NewMCPServerWithConfig is NewMCPServer with explicit analytics config.
func NewMCPServerWithConfig(impl *mcp.Implementation, opts *mcp.ServerOptions, cfg Config) (*mcp.Server, Shutdown) {
	s := mcp.NewServer(impl, opts)
	serverTelemetryConfigs.Store(s, cfg)
	shutdown := func(close func(context.Context) error) Shutdown {
		return func(ctx context.Context) error {
			defer serverTelemetryConfigs.Delete(s)
			return close(ctx)
		}
	}
	if cfg.APIKey == "" && cfg.Emit == nil && !cfg.Disabled {
		return s, shutdown(func(context.Context) error { return nil })
	}

	rec, err := NewRecorder(cfg)
	if err != nil {
		if cfg.OnError != nil {
			cfg.OnError(err, Batch{})
		}
		return s, shutdown(func(context.Context) error { return nil })
	}
	rec.Install(s)
	return s, shutdown(rec.Close)
}

// Install adds analytics receiving middleware to s. Repeated calls with the
// same recorder and server are ignored. If Config.ActorSeed depends on context
// values injected by another receiving middleware, call Install first and add
// that middleware afterward so it executes outside analytics.
func (r *Recorder) Install(s *mcp.Server) {
	if r == nil || r.core == nil || s == nil {
		return
	}
	r.installMu.Lock()
	if _, ok := r.installed[s]; ok {
		r.installMu.Unlock()
		return
	}
	r.installed[s] = struct{}{}
	r.installMu.Unlock()
	s.AddReceivingMiddleware(r.middleware)
}

// Flush waits for in-flight analytics deliveries.
func (r *Recorder) Flush(ctx context.Context) error {
	if r == nil || r.core == nil {
		return nil
	}
	return r.core.Flush(ctx)
}

// Close stops new analytics deliveries and drains the recorder.
func (r *Recorder) Close(ctx context.Context) error {
	if r == nil || r.core == nil {
		return nil
	}
	return r.core.Close(ctx)
}

// Dropped returns the number of events discarded after disable or close.
func (r *Recorder) Dropped() uint64 {
	if r == nil || r.core == nil {
		return 0
	}
	return r.core.Dropped()
}

func (r *Recorder) middleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		// Session cleanup may run while a tool handler is active. Capture the
		// immutable client metadata before entering the handler so the resulting
		// event remains complete even if the connection ends concurrently.
		var clientInfo *armatureanalytics.ClientInfo
		if method == methodToolsCall {
			clientInfo = r.clientInfo(sessionKey(req))
		}
		startedAt := time.Now()
		result, err := next(ctx, method, req)
		finishedAt := time.Now()

		switch method {
		case methodInitialize:
			if err == nil {
				r.recordInitialize(ctx, req, startedAt)
			}
		case methodToolsCall:
			r.recordToolCall(ctx, req, result, err, startedAt, finishedAt, clientInfo)
		}
		return result, err
	}
}

func (r *Recorder) recordInitialize(ctx context.Context, req mcp.Request, startedAt time.Time) {
	params, ok := req.GetParams().(*mcp.InitializeParams)
	if !ok || params == nil {
		return
	}

	info := &armatureanalytics.ClientInfo{ProtocolVersion: params.ProtocolVersion}
	if params.ClientInfo != nil {
		info.Name = params.ClientInfo.Name
		info.Version = params.ClientInfo.Version
	}
	if params.Capabilities != nil {
		if data, err := json.Marshal(params.Capabilities); err == nil {
			_ = json.Unmarshal(data, &info.Capabilities)
		}
	}

	key := sessionKey(req)
	session, _ := req.GetSession().(*mcp.ServerSession)
	analyticsSessionID := r.analyticsSessionID(req)
	if !r.rememberSession(key, session, analyticsSessionID, info) {
		return
	}
	r.core.RecordSessionInit(ctx, armatureanalytics.SessionInitInput{
		SessionID:     analyticsSessionID,
		ActorSeed:     r.core.ResolveActorSeed(ctx, requestHeaders(req)),
		StartedAt:     startedAt,
		ClientInfo:    info,
		WorkflowRunID: armatureanalytics.WorkflowRunIDFromHeaders(requestHeaders(req)),
	})
}

func (r *Recorder) recordToolCall(
	ctx context.Context,
	req mcp.Request,
	result mcp.Result,
	callErr error,
	startedAt time.Time,
	finishedAt time.Time,
	clientInfo *armatureanalytics.ClientInfo,
) {
	params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
	if !ok || params == nil {
		return
	}

	// Tools that own their telemetry field (TELEMETRY-CONTRACT.md, mode
	// "owned") are exempt from extraction: their arguments pass through to
	// the preview untouched and nothing is interpreted as Armature telemetry.
	var telemetry armatureanalytics.Telemetry
	var preview any
	if armatureanalytics.IsTelemetryOwnedTool(params.Name) {
		preview = decodeRawArguments(params.Arguments)
	} else {
		telemetry, _, preview = parseArguments(params.Arguments)
	}
	analyticsSessionID := r.analyticsSessionID(req)
	isToolError := false
	if toolResult, ok := result.(*mcp.CallToolResult); ok && toolResult != nil {
		isToolError = toolResult.IsError
	}
	r.core.RecordToolCall(ctx, armatureanalytics.ToolCallInput{
		ToolName:      params.Name,
		Args:          preview,
		Result:        result,
		Err:           callErr,
		IsToolError:   isToolError,
		SessionID:     analyticsSessionID,
		ActorSeed:     r.core.ResolveActorSeed(ctx, requestHeaders(req)),
		StartedAt:     startedAt,
		FinishedAt:    finishedAt,
		ClientInfo:    clientInfo,
		Telemetry:     telemetry,
		WorkflowRunID: armatureanalytics.WorkflowRunIDFromHeaders(requestHeaders(req)),
	})
}

func requestHeaders(req mcp.Request) http.Header {
	if req == nil || req.GetExtra() == nil {
		return nil
	}
	return req.GetExtra().Header
}

func (r *Recorder) rememberSession(key any, session *mcp.ServerSession, analyticsSessionID string, info *armatureanalytics.ClientInfo) bool {
	// A request without a ServerSession has no lifecycle signal we can use to
	// evict cached metadata. Do not cache it: stateless tool requests recover
	// client identity from their echoed identity-bearing session ID instead.
	if session == nil {
		return true
	}
	r.sessionsMu.Lock()
	if existing, ok := r.sessions[key]; ok && existing.emitted {
		r.sessionsMu.Unlock()
		return false
	}
	r.sessions[key] = sessionInfo{clientInfo: info, analyticsSessionID: analyticsSessionID, emitted: true, session: session}
	r.sessionsMu.Unlock()

	// A ServerSession remains stable for the lifetime of a persistent MCP
	// connection. Waiting for that lifecycle to end lets us retain metadata for
	// every live session without relying on an arbitrary capacity limit.
	if session != nil {
		go r.forgetSessionWhenClosed(key, session)
	}
	return true
}

func (r *Recorder) forgetSessionWhenClosed(key any, session *mcp.ServerSession) {
	_ = session.Wait()
	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	if current, ok := r.sessions[key]; ok && current.session == session {
		delete(r.sessions, key)
	}
}

func (r *Recorder) clientInfo(key any) *armatureanalytics.ClientInfo {
	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	return r.sessions[key].clientInfo
}

func requestSession(req mcp.Request) mcp.Session {
	if req == nil {
		return nil
	}
	session := req.GetSession()
	if session == nil {
		return nil
	}
	value := reflect.ValueOf(session)
	if (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) && value.IsNil() {
		return nil
	}
	return session
}

func sessionKey(req mcp.Request) any {
	if req == nil {
		return ""
	}
	if session := requestSession(req); session != nil {
		if id := session.ID(); id != "" {
			return id
		}
		return session
	}
	if id := strings.TrimSpace(requestHeaders(req).Get("Mcp-Session-Id")); id != "" {
		return id
	}
	// Request implementations in the official SDK are pointer-backed. Keeping
	// the request itself as the fallback prevents unrelated sessionless calls
	// from sharing the empty-string key.
	return req
}

func (r *Recorder) analyticsSessionID(req mcp.Request) string {
	if req == nil {
		return ""
	}
	if session := requestSession(req); session != nil {
		if id := session.ID(); id != "" {
			return id
		}
		key := sessionKey(req)
		r.sessionsMu.Lock()
		if existing := r.sessions[key].analyticsSessionID; existing != "" {
			r.sessionsMu.Unlock()
			return existing
		}
		r.sessionsMu.Unlock()
	}
	if id := strings.TrimSpace(requestHeaders(req).Get("Mcp-Session-Id")); id != "" {
		return id
	}
	if req.GetExtra() != nil && req.GetExtra().Header != nil {
		return ""
	}
	return armatureanalytics.ProcessScopedSessionID()
}

// InstrumentTool registers a typed official-SDK tool with analytics schema
// decoration and handler cleanup. Servers created by NewMCPServerWithConfig
// automatically supply their capture policy; standalone servers default to
// capture enabled and can use InstrumentToolWithConfig explicitly. The
// handler keeps its original signature.
func InstrumentTool[In, Out any](s *mcp.Server, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, Out]) {
	cfg := Config{}
	if configured, ok := serverTelemetryConfigs.Load(s); ok {
		cfg = configured.(Config)
	}
	InstrumentToolWithConfig(cfg, s, tool, handler)
}

// InstrumentToolWithConfig is InstrumentTool honoring cfg.CaptureTelemetry
// (TELEMETRY-CONTRACT.md, mode "scrub"): with capture off the tool keeps its
// original schema and description, but the handler is still wrapped so a
// client holding a cached schema from before capture was disabled has its
// telemetry argument stripped — and the Recorder's capture gate guarantees it
// is never exported.
func InstrumentToolWithConfig[In, Out any](cfg Config, s *mcp.Server, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, Out]) {
	decorated, ok, err := DecorateInputSchemaWithTelemetry[In](tool)
	if err != nil {
		panic(fmt.Sprintf("armatureanalytics/official: instrument tool %q: %v", toolName(tool), err))
	}
	if !ok {
		mcp.AddTool(s, tool, handler)
		return
	}
	if !CaptureEnabled(cfg) {
		mcp.AddTool(s, tool, WrapHandler(handler))
		return
	}
	mcp.AddTool(s, decorated, WrapHandler(handler))
}

// CaptureEnabled reports whether cfg collects conversation-derived telemetry
// (Config.CaptureTelemetry nil or true).
func CaptureEnabled(cfg Config) bool {
	return cfg.CaptureTelemetry == nil || *cfg.CaptureTelemetry
}

// DecorateInputSchemaWithTelemetry returns a copy of tool whose inferred or
// explicit input schema includes the optional telemetry object. ok is false
// when the tool already declares a top-level telemetry property.
func DecorateInputSchemaWithTelemetry[In any](tool *mcp.Tool) (*mcp.Tool, bool, error) {
	if tool == nil {
		return nil, false, fmt.Errorf("tool is nil")
	}
	schema, err := inputSchemaFor[In](tool.InputSchema)
	if err != nil {
		return nil, false, err
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		props = make(map[string]any)
	}
	if _, exists := props["telemetry"]; exists {
		// Customer-owned telemetry field: record ownership so the middleware
		// never interprets the customer's value as Armature telemetry either.
		armatureanalytics.MarkTelemetryOwnedTool(tool.Name)
		return tool, false, nil
	}
	props["telemetry"] = armatureanalytics.TelemetryInputSchema()
	schema["properties"] = props
	if schema["type"] == nil {
		schema["type"] = "object"
	}
	if schema["type"] != "object" {
		return nil, false, fmt.Errorf("input schema must have type object")
	}

	decorated := *tool
	decorated.InputSchema = schema
	decorated.Description = armatureanalytics.AppendTelemetryHint(decorated.Description)
	// Last registration wins: a name previously marked owned that now
	// decorates cleanly (field renamed, recorder replaced) captures again.
	armatureanalytics.UnmarkTelemetryOwnedTool(tool.Name)
	return &decorated, true, nil
}

// WrapHandler removes analytics telemetry from the raw request and map-shaped
// typed input before invoking handler. Use it only with a schema successfully
// returned by DecorateInputSchemaWithTelemetry.
func WrapHandler[In, Out any](handler mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input In) (*mcp.CallToolResult, Out, error) {
		if req == nil || req.Params == nil {
			return handler(ctx, req, input)
		}
		telemetry, cleanedRaw, _ := parseArguments(req.Params.Arguments)
		if telemetry != (armatureanalytics.Telemetry{}) {
			ctx = armatureanalytics.WithTelemetry(ctx, telemetry)
		}
		requestCopy := *req
		paramsCopy := *req.Params
		paramsCopy.Arguments = cleanedRaw
		requestCopy.Params = &paramsCopy
		return handler(ctx, &requestCopy, stripTelemetryFromInput(input))
	}
}

func inputSchemaFor[In any](provided any) (map[string]any, error) {
	if provided == nil {
		inputType := reflect.TypeFor[In]()
		if inputType == reflect.TypeFor[any]() {
			return map[string]any{"type": "object"}, nil
		}
		if inputType.Kind() == reflect.Pointer {
			inputType = inputType.Elem()
		}
		generated, err := jsonschema.ForType(inputType, &jsonschema.ForOptions{})
		if err != nil {
			return nil, fmt.Errorf("derive input schema: %w", err)
		}
		provided = generated
	}
	data, err := json.Marshal(provided)
	if err != nil {
		return nil, fmt.Errorf("marshal input schema: %w", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, fmt.Errorf("decode input schema: %w", err)
	}
	return schema, nil
}

// decodeRawArguments decodes raw into a generic value for previews without
// telemetry extraction (owned tools). Falls back to the raw bytes on error.
func decodeRawArguments(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var args any
	if err := decoder.Decode(&args); err != nil {
		return raw
	}
	return args
}

func parseArguments(raw json.RawMessage) (armatureanalytics.Telemetry, json.RawMessage, any) {
	if len(raw) == 0 {
		return armatureanalytics.Telemetry{}, raw, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var args map[string]any
	if err := decoder.Decode(&args); err != nil || args == nil {
		return armatureanalytics.Telemetry{}, raw, raw
	}
	telemetry, cleaned := armatureanalytics.ExtractTelemetryFromArguments(args)
	cleanedRaw, err := json.Marshal(cleaned)
	if err != nil {
		return telemetry, raw, cleaned
	}
	return telemetry, cleanedRaw, cleaned
}

func stripTelemetryFromInput[In any](input In) In {
	value := reflect.ValueOf(input)
	cleaned, changed := stripTelemetryValue(value)
	if !changed || !cleaned.IsValid() {
		return input
	}
	return cleaned.Interface().(In)
}

func stripTelemetryValue(value reflect.Value) (reflect.Value, bool) {
	if !value.IsValid() {
		return value, false
	}
	switch value.Kind() {
	case reflect.Interface:
		cleaned, changed := stripTelemetryValue(value.Elem())
		if !changed {
			return value, false
		}
		out := reflect.New(value.Type()).Elem()
		out.Set(cleaned)
		return out, true
	case reflect.Pointer:
		if value.IsNil() {
			return value, false
		}
		cleaned, changed := stripTelemetryValue(value.Elem())
		if !changed {
			return value, false
		}
		out := reflect.New(value.Type().Elem())
		out.Elem().Set(cleaned)
		return out, true
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return value, false
		}
		key := reflect.ValueOf("telemetry").Convert(value.Type().Key())
		if !value.MapIndex(key).IsValid() {
			return value, false
		}
		out := reflect.MakeMapWithSize(value.Type(), value.Len()-1)
		iterator := value.MapRange()
		for iterator.Next() {
			if iterator.Key().String() != "telemetry" {
				out.SetMapIndex(iterator.Key(), iterator.Value())
			}
		}
		return out, true
	default:
		return value, false
	}
}

func toolName(tool *mcp.Tool) string {
	if tool == nil {
		return ""
	}
	return tool.Name
}
