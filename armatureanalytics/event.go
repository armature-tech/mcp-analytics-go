package armatureanalytics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Schema-version and size limits mirror the @armature-tech/mcp-analytics TS
// SDK so payloads land in the same Armature ingest pipeline regardless of
// origin language.
const (
	SchemaVersion = 1

	KindToolCall      = "tool_call"
	KindSessionInit   = "session_init"
	KindActorIdentity = "actor_identity"

	MaxPreviewBytes      = 8 * 1024
	MaxSourceBytes       = 32 * 1024
	MaxCapabilitiesBytes = 4 * 1024

	anonymousActor = "anonymous"
)

// Batch is the JSON envelope POSTed to the Armature ingest endpoint.
type Batch struct {
	SchemaVersion int     `json:"schema_version"`
	Events        []Event `json:"events"`
}

// Event is one entry in a Batch. Kind is "tool_call", "session_init", or "actor_identity".
// The shape mirrors src/events.ts in @armature-tech/mcp-analytics.
type Event struct {
	EventID               string         `json:"event_id"`
	Kind                  string         `json:"kind"`
	ActorID               string         `json:"actor_id"`
	SessionIDHint         *string        `json:"session_id_hint"`
	StartedAt             string         `json:"started_at"`
	FinishedAt            string         `json:"finished_at"`
	DurationMs            int64          `json:"duration_ms"`
	OK                    bool           `json:"ok"`
	Error                 *string        `json:"error"`
	Metadata              map[string]any `json:"metadata"`
	ScriptSource          *string        `json:"script_source"`
	ScriptSourceTruncated bool           `json:"script_source_truncated"`
	ResultPreview         *string        `json:"result_preview"`
	ResultTruncated       bool           `json:"result_truncated"`
	Calls                 []any          `json:"calls"`
	Logs                  []any          `json:"logs"`
	SearchCalls           []any          `json:"search_calls"`
	IsWorkflow            bool           `json:"is_workflow,omitempty"`
	WorkflowRunID         string         `json:"workflow_run_id,omitempty"`
}

// BuildActorIdentityEvent constructs a content-addressed actor profile update.
func BuildActorIdentityEvent(actorID string, identifier string, startedAt time.Time) Event {
	stamp := startedAt.UTC().Format(time.RFC3339Nano)
	return Event{
		EventID:     EventID(actorID, KindActorIdentity, identifier),
		Kind:        KindActorIdentity,
		ActorID:     actorID,
		StartedAt:   stamp,
		FinishedAt:  stamp,
		OK:          true,
		Metadata:    map[string]any{"identifier": identifier},
		Calls:       []any{},
		Logs:        []any{},
		SearchCalls: []any{},
	}
}

// ToolCallInput is the typed input to BuildToolCallEvent — what every hook
// integration needs to surface for a single MCP tool call.
type ToolCallInput struct {
	ToolName      string
	RequestID     string // optional explicit idempotency key; defaults to a fresh UUID
	Args          any
	Result        any
	Err           error
	IsToolError   bool   // set when the tool returned an MCP error result without raising err
	ErrorType     string // optional classification (e.g. "auth_failed"); falls back to err.Error()
	SessionID     string
	ActorSeed     string // typically the auth principal; hashed into actor_id
	StartedAt     time.Time
	FinishedAt    time.Time
	ClientInfo    *ClientInfo
	Telemetry     Telemetry // optional LLM-supplied telemetry (V1 or pre-V1 spellings; normalized on emit)
	WorkflowRunID string    // optional Armature workflow-run UUID; marks synthetic traffic
	// CapabilityRequest marks SDK-owned request_capability calls so ingest can
	// distinguish them from a customer tool that happens to use the same name.
	CapabilityRequest bool
	// Redact runs over the sanitized args/result (and the normalized telemetry
	// and error string) before serialization. Recorder.RecordToolCall fills it
	// from Config.Redact; direct BuildToolCallEvent callers may set it
	// themselves. A panicking hook fails closed to "[redaction failed]".
	Redact func(any) any
	// RedactSecrets defaults to true when nil.
	RedactSecrets           *bool
	actorHeaders            http.Header
	actorIdentifier         string
	actorIdentifierResolved bool
}

// RedactableToolCall is the context-rich candidate passed to RedactEvent.
type RedactableToolCall struct {
	Kind         string     `json:"kind"`
	ToolName     string     `json:"toolName"`
	Status       string     `json:"status"`
	DurationMs   int64      `json:"durationMs"`
	SessionID    string     `json:"sessionId,omitempty"`
	Input        any        `json:"input"`
	Output       any        `json:"output,omitempty"`
	ErrorMessage *string    `json:"errorMessage,omitempty"`
	Telemetry    *Telemetry `json:"telemetry,omitempty"`
}

// RedactEventHook runs after built-in and legacy per-value redaction but
// before serialization and truncation.
type RedactEventHook func(context.Context, *RedactableToolCall) (*RedactableToolCall, error)

// SessionInitInput is the typed input to BuildSessionInitEvent.
type SessionInitInput struct {
	SessionID               string
	ActorSeed               string
	StartedAt               time.Time
	ClientInfo              *ClientInfo
	WorkflowRunID           string // optional Armature workflow-run UUID; marks synthetic traffic
	actorHeaders            http.Header
	actorIdentifier         string
	actorIdentifierResolved bool
}

// ClientInfo mirrors the MCP InitializeRequest's clientInfo block plus
// optional capabilities.
type ClientInfo struct {
	Name            string
	Version         string
	ProtocolVersion string
	Capabilities    map[string]any
}

// BuildToolCallEvent constructs the wire-shape Event for a single tool call.
func BuildToolCallEvent(in ToolCallInput) Event {
	candidate := prepareToolCallCandidate(in)
	return assembleToolCallEvent(in, candidate)
}

// FinalizeToolCallEvent prepares a candidate and applies the whole-event hook.
// A nil return intentionally drops the tool event; hook failures fail closed.
func FinalizeToolCallEvent(ctx context.Context, in ToolCallInput, hook RedactEventHook) *Event {
	candidate := prepareToolCallCandidate(in)
	if hook != nil {
		redacted, err := callRedactEvent(ctx, hook, candidate)
		if err == nil && redacted == nil {
			return nil
		}
		if err != nil {
			failed := RedactionFailedPlaceholder
			candidate.Input = RedactionFailedPlaceholder
			candidate.Output = RedactionFailedPlaceholder
			candidate.ErrorMessage = &failed
			candidate.Telemetry = nil
		} else {
			candidate = redacted
		}
	}
	event := assembleToolCallEvent(in, candidate)
	return &event
}

func callRedactEvent(ctx context.Context, hook RedactEventHook, candidate *RedactableToolCall) (redacted *RedactableToolCall, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("redaction hook failed")
			redacted = nil
		}
	}()
	return hook(ctx, candidate)
}

func prepareToolCallCandidate(in ToolCallInput) *RedactableToolCall {
	ok := in.Err == nil && !in.IsToolError
	status := "ok"
	if !ok {
		status = "error"
	}
	var errPtr *string
	if !ok {
		msg := in.ErrorType
		if msg == "" {
			if in.Err != nil {
				msg = in.Err.Error()
			} else {
				msg = "tool_error"
			}
		}
		prepared := prepareErrorMessage(msg, in.Redact, secretsEnabled(in.RedactSecrets))
		errPtr = &prepared
	}

	candidate := &RedactableToolCall{
		Kind:         KindToolCall,
		ToolName:     in.ToolName,
		Status:       status,
		DurationMs:   in.FinishedAt.Sub(in.StartedAt).Milliseconds(),
		SessionID:    in.SessionID,
		Input:        prepareForPreview(in.Args, in.Redact, secretsEnabled(in.RedactSecrets)),
		ErrorMessage: errPtr,
	}
	if in.Result != nil {
		candidate.Output = prepareForPreview(in.Result, in.Redact, secretsEnabled(in.RedactSecrets))
	}
	if in.Telemetry != (Telemetry{}) {
		candidate.Telemetry = prepareTelemetry(in.Telemetry, in.Redact, secretsEnabled(in.RedactSecrets))
	}
	return candidate
}

func assembleToolCallEvent(in ToolCallInput, candidate *RedactableToolCall) Event {
	actorID := ActorID(in.ActorSeed)
	requestID := in.RequestID
	if requestID == "" {
		requestID = randomUUID()
	}
	source, sourceTrunc := truncateUTF8("MCP tool call: "+candidate.ToolName+"\n\nInput:\n"+stringifyPreview(candidate.Input), MaxSourceBytes)
	inputPreview, _ := truncateUTF8(stringifyPreview(candidate.Input), MaxPreviewBytes)
	var resultPtr *string
	var resultTrunc bool
	if candidate.Output != nil {
		preview, trunc := truncateUTF8(stringifyPreview(candidate.Output), MaxPreviewBytes)
		resultPtr = &preview
		resultTrunc = trunc
	}
	tel := Telemetry{}
	if candidate.Telemetry != nil {
		tel = NormalizeTelemetry(*candidate.Telemetry)
	}
	meta := map[string]any{
		"tool_name":         candidate.ToolName,
		"user_intent":       stringOrNil(tel.UserIntent),
		"agent_thinking":    stringOrNil(tel.AgentThinking),
		"user_frustration":  stringOrNil(tel.UserFrustration),
		"intent":            stringOrNil(tel.UserIntent),
		"context":           stringOrNil(tel.AgentThinking),
		"frustration_level": stringOrNil(tel.UserFrustration),
		"input_preview":     inputPreview,
	}
	if in.CapabilityRequest {
		meta["capability_request"] = true
	}
	mergeClientInfo(meta, in.ClientInfo)
	return Event{
		EventID:               EventID(actorID, KindToolCall, requestID),
		Kind:                  KindToolCall,
		ActorID:               actorID,
		SessionIDHint:         stringPtrOrNil(candidate.SessionID),
		StartedAt:             in.StartedAt.UTC().Format(time.RFC3339Nano),
		FinishedAt:            in.FinishedAt.UTC().Format(time.RFC3339Nano),
		DurationMs:            candidate.DurationMs,
		OK:                    candidate.Status == "ok",
		Error:                 candidate.ErrorMessage,
		Metadata:              meta,
		ScriptSource:          &source,
		ScriptSourceTruncated: sourceTrunc,
		ResultPreview:         resultPtr,
		ResultTruncated:       resultTrunc,
		Calls:                 []any{},
		Logs:                  []any{},
		SearchCalls:           []any{},
		IsWorkflow:            in.WorkflowRunID != "",
		WorkflowRunID:         in.WorkflowRunID,
	}
}

func secretsEnabled(configured *bool) bool {
	return configured == nil || *configured
}

func prepareForPreview(value any, redact func(any) any, redactSecrets bool) any {
	prepared := SanitizeValue(value)
	if redactSecrets {
		prepared = RedactSecretsInValue(prepared)
	}
	if redact == nil {
		return prepared
	}
	redacted, ok := callLegacyRedact(redact, prepared)
	if !ok {
		return RedactionFailedPlaceholder
	}
	return redacted
}

func prepareErrorMessage(message string, redact func(any) any, redactSecrets bool) string {
	if redactSecrets {
		message = RedactSecretsInString(message)
	}
	if redact == nil {
		return message
	}
	redacted, ok := callLegacyRedact(redact, message)
	if !ok {
		return RedactionFailedPlaceholder
	}
	if text, ok := redacted.(string); ok {
		return text
	}
	return stringifyPreview(redacted)
}

func prepareTelemetry(telemetry Telemetry, redact func(any) any, redactSecrets bool) *Telemetry {
	telemetry = NormalizeTelemetry(telemetry)
	if redactSecrets {
		telemetry.UserIntent = RedactSecretsInString(telemetry.UserIntent)
		telemetry.AgentThinking = RedactSecretsInString(telemetry.AgentThinking)
		telemetry.Intent = telemetry.UserIntent
		telemetry.Context = telemetry.AgentThinking
	}
	if redact == nil {
		return &telemetry
	}
	redacted, ok := callLegacyRedact(redact, telemetry)
	if !ok {
		return nil
	}
	converted, ok := telemetryFromAny(redacted)
	if !ok {
		return nil
	}
	converted = NormalizeTelemetry(converted)
	return &converted
}

func callLegacyRedact(redact func(any) any, value any) (redacted any, ok bool) {
	defer func() {
		if recover() != nil {
			redacted = nil
			ok = false
		}
	}()
	return redact(value), true
}

func telemetryFromAny(value any) (Telemetry, bool) {
	if telemetry, ok := value.(Telemetry); ok {
		return telemetry, true
	}
	if telemetry, ok := value.(*Telemetry); ok && telemetry != nil {
		return *telemetry, true
	}
	data, err := json.Marshal(value)
	if err != nil {
		return Telemetry{}, false
	}
	var telemetry Telemetry
	if err := json.Unmarshal(data, &telemetry); err != nil {
		return Telemetry{}, false
	}
	return telemetry, true
}

// BuildSessionInitEvent constructs the wire-shape Event for a session-init.
func BuildSessionInitEvent(in SessionInitInput) Event {
	actorID := ActorID(in.ActorSeed)
	requestID := actorID + ":session_init:" + in.SessionID
	stamp := in.StartedAt.UTC().Format(time.RFC3339Nano)

	meta := map[string]any{}
	mergeClientInfo(meta, in.ClientInfo)
	// Always present the four canonical keys, with nil when unset, so that
	// downstream consumers can rely on field presence.
	if _, ok := meta["client_name"]; !ok {
		meta["client_name"] = nil
	}
	if _, ok := meta["client_version"]; !ok {
		meta["client_version"] = nil
	}
	if _, ok := meta["protocol_version"]; !ok {
		meta["protocol_version"] = nil
	}
	if _, ok := meta["capabilities"]; !ok {
		meta["capabilities"] = nil
	}

	return Event{
		EventID:       EventID(actorID, KindSessionInit, requestID),
		Kind:          KindSessionInit,
		ActorID:       actorID,
		SessionIDHint: stringPtrOrNil(in.SessionID),
		StartedAt:     stamp,
		FinishedAt:    stamp,
		DurationMs:    0,
		OK:            true,
		Error:         nil,
		Metadata:      meta,
		ScriptSource:  nil,
		ResultPreview: nil,
		Calls:         []any{},
		Logs:          []any{},
		SearchCalls:   []any{},
		IsWorkflow:    in.WorkflowRunID != "",
		WorkflowRunID: in.WorkflowRunID,
	}
}

// mergeClientInfo populates the common client_name / client_version /
// protocol_version / capabilities keys when the input is non-nil.
func mergeClientInfo(meta map[string]any, ci *ClientInfo) {
	if ci == nil {
		return
	}
	if ci.Name != "" {
		meta["client_name"] = ci.Name
	}
	if ci.Version != "" {
		meta["client_version"] = ci.Version
	}
	if ci.ProtocolVersion != "" {
		meta["protocol_version"] = ci.ProtocolVersion
	}
	if len(ci.Capabilities) > 0 {
		if data, err := json.Marshal(ci.Capabilities); err == nil && len(data) <= MaxCapabilitiesBytes {
			meta["capabilities"] = ci.Capabilities
		}
	}
}

// ActorID returns the sha256 hex of the supplied seed, or sha256("anonymous")
// when the seed is empty. Mirrors the TS SDK fallback semantics.
func ActorID(seed string) string {
	if seed == "" {
		seed = anonymousActor
	}
	return sha256Hex(seed)
}

// EventID returns a deterministic ID derived from (actorID, kind, requestID).
// Matches the TS SDK shape: sha256(actor_id + " " + kind + " " + request_id).
func EventID(actorID, kind, requestID string) string {
	return sha256Hex(actorID + " " + kind + " " + requestID)
}

func sha256Hex(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// stringifyPreview marshals a value to a single-line JSON preview. Returns
// "undefined" when v is nil to mirror the TS SDK's undefined-handling and
// "[unserialisable]" on marshal failure.
func stringifyPreview(v any) string {
	if v == nil {
		return "undefined"
	}
	data, err := json.Marshal(v)
	if err != nil {
		return "[unserialisable]"
	}
	return string(data)
}

// truncateUTF8 returns s capped at maxBytes UTF-8 bytes plus a "was truncated"
// flag. Slicing happens on a rune boundary so the result is always valid UTF-8.
func truncateUTF8(s string, maxBytes int) (string, bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	// Walk back from maxBytes to a rune boundary.
	i := maxBytes
	for i > 0 && !isUTF8RuneStart(s[i]) {
		i--
	}
	return s[:i], true
}

func isUTF8RuneStart(b byte) bool {
	// ASCII or non-continuation byte starts a rune.
	return b < 0x80 || b >= 0xC0
}

func stringPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// stringOrNil returns the string when non-empty, otherwise nil. Used for
// metadata fields that the wire schema expects as `string | null`.
func stringOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}
