package armatureanalytics

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Schema-version and size limits mirror the @armature-tech/mcp-analytics TS
// SDK so payloads land in the same Armature ingest pipeline regardless of
// origin language.
const (
	SchemaVersion = 1

	KindToolCall    = "tool_call"
	KindSessionInit = "session_init"

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

// Event is one entry in a Batch. Kind is "tool_call" or "session_init".
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
	// Redact runs over the sanitized args/result (and the normalized telemetry
	// and error string) before serialization. Recorder.RecordToolCall fills it
	// from Config.Redact; direct BuildToolCallEvent callers may set it
	// themselves. A panicking hook fails closed to "[redaction failed]".
	Redact func(any) any
}

// SessionInitInput is the typed input to BuildSessionInitEvent.
type SessionInitInput struct {
	SessionID     string
	ActorSeed     string
	StartedAt     time.Time
	ClientInfo    *ClientInfo
	WorkflowRunID string // optional Armature workflow-run UUID; marks synthetic traffic
}

// ClientInfo mirrors the MCP InitializeRequest's clientInfo block plus
// optional capabilities.
type ClientInfo struct {
	Name            string
	Version         string
	ProtocolVersion string
	Capabilities    map[string]any
}

// redactTelemetry runs the customer redaction hook over the normalized
// telemetry. Telemetry text is agent-authored but routinely quotes the user,
// so the hook sees it too; whatever it returns is re-normalized, and a
// panicking hook drops the telemetry entirely (fail closed).
func redactTelemetry(t Telemetry, redact func(any) any) Telemetry {
	if redact == nil || t == (Telemetry{}) {
		return t
	}
	generic, ok := toGenericJSON(t)
	if !ok {
		return Telemetry{}
	}
	redacted := safeRedact(redact, generic)
	data, err := json.Marshal(redacted)
	if err != nil {
		return Telemetry{}
	}
	var out Telemetry
	if err := json.Unmarshal(data, &out); err != nil {
		return Telemetry{}
	}
	return NormalizeTelemetry(out)
}

func redactErrorMessage(message string, redact func(any) any) string {
	if redact == nil || message == "" {
		return message
	}
	redacted := safeRedact(redact, message)
	if s, ok := redacted.(string); ok {
		return s
	}
	return stringifyPreview(redacted)
}

// BuildToolCallEvent constructs the wire-shape Event for a single tool call.
func BuildToolCallEvent(in ToolCallInput) Event {
	actorID := ActorID(in.ActorSeed)
	requestID := in.RequestID
	if requestID == "" {
		requestID = randomUUID()
	}

	// Contract pipeline (TELEMETRY-CONTRACT.md): sanitize → customer redact →
	// stringify → truncate, for every payload that can carry customer data —
	// input preview, the source built from the input, the result preview, the
	// error string, and the telemetry text.
	safeArgs := prepareForPreview(in.Args, in.Redact)
	source, sourceTrunc := truncateUTF8("MCP tool call: "+in.ToolName+"\n\nInput:\n"+stringifyPreview(safeArgs), MaxSourceBytes)
	inputPreview, _ := truncateUTF8(stringifyPreview(safeArgs), MaxPreviewBytes)
	var resultPtr *string
	var resultTrunc bool
	if in.Result != nil {
		preview, trunc := truncateUTF8(stringifyPreview(prepareForPreview(in.Result, in.Redact)), MaxPreviewBytes)
		resultPtr = &preview
		resultTrunc = trunc
	}

	ok := in.Err == nil && !in.IsToolError
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
		msg = redactErrorMessage(msg, in.Redact)
		errPtr = &msg
	}

	// Canonicalize onto the V1 field names (legacy input spellings accepted)
	// and emit both key sets: the V1 keys plus legacy mirrors, so an ingest
	// that hasn't picked up the V1 schema keeps reading events from this SDK.
	tel := redactTelemetry(NormalizeTelemetry(in.Telemetry), in.Redact)
	meta := map[string]any{
		"tool_name":         in.ToolName,
		"user_intent":       stringOrNil(tel.UserIntent),
		"agent_thinking":    stringOrNil(tel.AgentThinking),
		"user_frustration":  stringOrNil(tel.UserFrustration),
		"intent":            stringOrNil(tel.UserIntent),
		"context":           stringOrNil(tel.AgentThinking),
		"frustration_level": stringOrNil(tel.UserFrustration),
		"input_preview":     inputPreview,
	}
	mergeClientInfo(meta, in.ClientInfo)

	return Event{
		EventID:               EventID(actorID, KindToolCall, requestID),
		Kind:                  KindToolCall,
		ActorID:               actorID,
		SessionIDHint:         stringPtrOrNil(in.SessionID),
		StartedAt:             in.StartedAt.UTC().Format(time.RFC3339Nano),
		FinishedAt:            in.FinishedAt.UTC().Format(time.RFC3339Nano),
		DurationMs:            in.FinishedAt.Sub(in.StartedAt).Milliseconds(),
		OK:                    ok,
		Error:                 errPtr,
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
