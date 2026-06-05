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
}

// ToolCallInput is the typed input to BuildToolCallEvent — what every hook
// integration needs to surface for a single MCP tool call.
type ToolCallInput struct {
	ToolName    string
	Args        any
	Result      any
	Err         error
	IsToolError bool   // set when the tool returned an MCP error result without raising err
	ErrorType   string // optional classification (e.g. "auth_failed"); falls back to err.Error()
	SessionID   string
	ActorSeed   string // typically the auth principal; hashed into actor_id
	StartedAt   time.Time
	FinishedAt  time.Time
	ClientInfo  *ClientInfo
}

// SessionInitInput is the typed input to BuildSessionInitEvent.
type SessionInitInput struct {
	SessionID  string
	ActorSeed  string
	StartedAt  time.Time
	ClientInfo *ClientInfo
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
	actorID := ActorID(in.ActorSeed)
	requestID := requestIDFor(actorID, KindToolCall, in.ToolName, in.FinishedAt)

	source, sourceTrunc := truncateUTF8("MCP tool call: "+in.ToolName+"\n\nInput:\n"+stringifyPreview(in.Args), MaxSourceBytes)
	inputPreview, _ := truncateUTF8(stringifyPreview(in.Args), MaxPreviewBytes)
	var resultPtr *string
	var resultTrunc bool
	if in.Result != nil {
		preview, trunc := truncateUTF8(stringifyPreview(in.Result), MaxPreviewBytes)
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
		errPtr = &msg
	}

	meta := map[string]any{
		"tool_name":         in.ToolName,
		"intent":            nil,
		"context":           nil,
		"frustration_level": nil,
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

func requestIDFor(actorID, kind, toolName string, finishedAt time.Time) string {
	return actorID + ":" + kind + ":" + toolName + ":" + finishedAt.UTC().Format(time.RFC3339Nano)
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
