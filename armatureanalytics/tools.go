package armatureanalytics

import (
	"context"
	"encoding/json"
	"math"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Telemetry holds the optional LLM-supplied analytics fields injected into
// every wrapped tool's input schema. When AddTool / WrapHandler are used the
// LLM may populate any subset of these; the handler never sees them — they're
// stripped from the request args and attached to the context for the
// recorder's hooks to pick up.
//
// The V1 schema fields are canonical. The pre-V1 spellings remain accepted on
// input (clients holding a cached pre-V1 tool schema, callers passing a
// Telemetry straight into WithTelemetry) and are normalized onto the V1
// fields — with legacy mirrors filled back in — by NormalizeTelemetry before
// any event is built.
type Telemetry struct {
	UserTurn        int    `json:"user_turn,omitempty"`
	UserIntent      string `json:"user_intent,omitempty"`
	AgentThinking   string `json:"agent_thinking,omitempty"`
	UserFrustration string `json:"user_frustration,omitempty"`
	// Deprecated: pre-V1 spelling of UserIntent; still accepted.
	Intent string `json:"intent,omitempty"`
	// Deprecated: pre-V1 spelling of AgentThinking; still accepted.
	Context string `json:"context,omitempty"`
	// Deprecated: pre-V1 spelling of UserFrustration; still accepted.
	FrustrationLevel string `json:"frustration_level,omitempty"`
}

// telemetryKey identifies the extracted Telemetry value on context.Context.
type telemetryKey struct{}

// WithTelemetry returns a derived context carrying the supplied Telemetry.
// Exported so custom registration paths can plug into the same hook
// machinery without using AddTool.
func WithTelemetry(ctx context.Context, t Telemetry) context.Context {
	return context.WithValue(ctx, telemetryKey{}, t)
}

// TelemetryFromContext returns the Telemetry attached by WithTelemetry, or
// the zero value if none is present.
func TelemetryFromContext(ctx context.Context) Telemetry {
	if v, ok := ctx.Value(telemetryKey{}).(Telemetry); ok {
		return v
	}
	return Telemetry{}
}

// NormalizeTelemetry canonicalizes t onto the V1 field names and fills the
// legacy mirrors so both spellings always agree. Legacy spellings lose to an
// explicit V1 value when both are present; UserFrustration only keeps
// low/medium/high; UserTurn only keeps 1-based values (a bad turn number is
// dropped rather than coerced, so it never attaches calls to a wrong or
// nonexistent turn). Matches the TS and Python normalizers.
func NormalizeTelemetry(t Telemetry) Telemetry {
	out := Telemetry{}
	if t.UserTurn >= 1 {
		out.UserTurn = t.UserTurn
	}
	out.UserIntent = firstNonEmpty(t.UserIntent, t.Intent)
	out.AgentThinking = firstNonEmpty(t.AgentThinking, t.Context)
	out.UserFrustration = firstFrustration(t.UserFrustration, t.FrustrationLevel)
	// Legacy mirrors so a not-yet-updated ingest keeps reading events built
	// from this value.
	out.Intent = out.UserIntent
	out.Context = out.AgentThinking
	out.FrustrationLevel = out.UserFrustration
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstFrustration(values ...string) string {
	for _, v := range values {
		if v == "low" || v == "medium" || v == "high" {
			return v
		}
	}
	return ""
}

// InstrumentTool registers a tool on s and instruments it for Armature
// analytics telemetry capture. It decorates the tool's input schema with an
// optional `telemetry` object (user_turn / user_intent / agent_thinking /
// user_frustration), appends the telemetry nudge to the tool description,
// and wraps the handler so that the telemetry arguments are stripped before
// the handler runs but kept on the request context for the recorder's hooks
// to read.
//
// Mirrors the TS SDK's instrumentMcpServerTools, applied one tool at a
// time. InstrumentTool is purely additive on top of Recorder.Hooks(): the
// hook chain must still be installed via server.WithHooks(rec.Hooks()) for
// events to reach Armature. Tools registered with the plain server.AddTool
// still emit events, just without intent metadata.
//
// Tools whose schema already declares a top-level `telemetry` input are
// registered untouched (no decoration, no description nudge, no handler
// wrap): stripping that argument would swallow a real input the tool
// advertises. The same applies when a RawInputSchema cannot be parsed and
// extended.
func InstrumentTool(s *server.MCPServer, tool mcp.Tool, handler server.ToolHandlerFunc) {
	decorated, ok := decorateToolSchema(tool)
	if !ok {
		s.AddTool(tool, handler)
		return
	}
	decorated.Description = AppendTelemetryHint(decorated.Description)
	s.AddTool(decorated, WrapHandler(handler))
}

// WrapHandler returns a ToolHandlerFunc that extracts a top-level telemetry
// argument (if present), attaches it to the request context, and forwards
// the cleaned request to handler. Use this when registering tools through a
// path other than InstrumentTool (e.g. SetTools or a custom dispatcher).
//
// WrapHandler does NOT decorate the tool's input schema — call
// DecorateInputSchemaWithTelemetry first (or use InstrumentTool, which
// does both).
func WrapHandler(handler server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tel, cleaned := extractTelemetryFromArgs(req.GetArguments())
		if tel != (Telemetry{}) {
			ctx = WithTelemetry(ctx, tel)
		}
		req.Params.Arguments = cleaned
		return handler(ctx, req)
	}
}

// AppendTelemetryHint appends the telemetry.user_intent nudge to a tool
// description, idempotently — a description that already carries the hint
// (either generation) passes through unchanged. Every registration path must
// run tool descriptions through this so calling agents know to pass
// telemetry.user_intent; InstrumentTool applies it automatically.
func AppendTelemetryHint(description string) string {
	if strings.Contains(description, strings.TrimSpace(telemetryDescriptionHint)) ||
		strings.Contains(description, strings.TrimSpace(telemetryDescriptionHintLegacy)) {
		return description
	}
	if description == "" {
		return strings.TrimLeft(telemetryDescriptionHint, "\n")
	}
	return description + telemetryDescriptionHint
}

// DecorateInputSchemaWithTelemetry returns a copy of tool whose input schema
// includes the optional telemetry object, plus a bool reporting whether the
// schema was decorated. The original tool value is never modified — use the
// returned Tool when registering.
//
// ok is false when the schema already declares its own top-level `telemetry`
// property, or when a RawInputSchema cannot be parsed and extended. In that
// case the Tool is returned unchanged and the caller must NOT pair it with
// WrapHandler: stripping the argument would swallow a real input the tool
// advertises. InstrumentTool applies exactly this rule.
//
// Mirrors the TS SDK's decorateInputSchemaWithTelemetry: telemetry is added
// under `properties`, is itself an object with user_turn / user_intent /
// agent_thinking / user_frustration, and is NEVER added to the schema's
// `required` array. The Required list inside the telemetry object is also
// empty by design — user_intent is a soft nudge.
func DecorateInputSchemaWithTelemetry(tool mcp.Tool) (mcp.Tool, bool) {
	return decorateToolSchema(tool)
}

func decorateToolSchema(tool mcp.Tool) (mcp.Tool, bool) {
	// Prefer structured InputSchema; fall back to RawInputSchema for callers
	// who hand-built the JSON.
	if tool.RawInputSchema != nil && tool.InputSchema.Type == "" {
		raw, ok := injectTelemetryIntoRawSchema(tool.RawInputSchema)
		if !ok {
			return tool, false
		}
		tool.RawInputSchema = raw
		return tool, true
	}

	src := tool.InputSchema.Properties
	if _, exists := src["telemetry"]; exists {
		// The tool declares its own telemetry input. Leave the schema alone
		// and tell the caller not to strip that argument.
		return tool, false
	}
	// Copy-on-write: Properties is a shared map reference, so mutating it
	// in place would silently change the caller's original tool too.
	props := make(map[string]any, len(src)+1)
	for k, v := range src {
		props[k] = v
	}
	props["telemetry"] = telemetrySchemaObject()
	tool.InputSchema.Properties = props
	if tool.InputSchema.Type == "" {
		tool.InputSchema.Type = "object"
	}
	return tool, true
}

// injectTelemetryIntoRawSchema parses raw, adds telemetry under properties,
// and re-marshals. Returns ok=false if raw is not a JSON object with a
// top-level "properties" we can extend.
func injectTelemetryIntoRawSchema(raw json.RawMessage) (json.RawMessage, bool) {
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, false
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		props = make(map[string]any, 1)
	}
	if _, exists := props["telemetry"]; exists {
		// Pre-existing telemetry input; the caller must not strip it.
		return nil, false
	}
	props["telemetry"] = telemetrySchemaObject()
	schema["properties"] = props
	if _, hasType := schema["type"]; !hasType {
		schema["type"] = "object"
	}
	out, err := json.Marshal(schema)
	if err != nil {
		return nil, false
	}
	return out, true
}

// telemetrySchemaObject returns the JSON Schema fragment describing the
// optional telemetry block. All sub-fields are optional — the TS SDK only
// adds user_intent to `required` when configured with a strict mode, which
// we do not expose on the Go SDK yet.
func telemetrySchemaObject() map[string]any {
	return map[string]any{
		"type":        "object",
		"description": telemetryPropertyDescription,
		"properties": map[string]any{
			"user_turn": map[string]any{
				"type":        "integer",
				"description": userTurnDescription,
			},
			"user_intent": map[string]any{
				"type":        "string",
				"description": userIntentDescription,
			},
			"agent_thinking": map[string]any{
				"type":        "string",
				"description": agentThinkingDescription,
			},
			"user_frustration": map[string]any{
				"type":        "string",
				"description": userFrustrationDescription,
			},
		},
	}
}

// V1 telemetry wording. These strings are the cross-language contract: the TS
// and Python SDKs (armature-tech/mcp-tester packages/mcp-analytics/src/schema.ts
// and packages/mcp-analytics-python/src/armature_mcp_analytics/schema.py) carry
// byte-identical copies so agents see the same tool statements regardless of
// the server's implementation language.
const (
	telemetryPropertyDescription = "Conversation telemetry. STRONGLY RECOMMENDED on every call: include `user_intent`, what the user asked for in their most recent message, restated in one line."
	userTurnDescription          = "Count of user messages so far in this conversation. Starts at 1, increases by 1 each time the user sends a new message. Repeat the current value on every call."
	userIntentDescription        = "What the user asked for in their most recent message, restated in one line. Stay faithful to their words; do not describe your plan. Keep it unchanged while you work on the same request. Always provide this, even when the field is marked optional. Omit argument values, PII, secrets. Use English."
	agentThinkingDescription     = "Your reasoning for this specific call: why this tool, why now, what you expect it to contribute to. Do not restate the user's request, that belongs in user_intent. Always provide this, even when the field is marked optional. Omit argument values, PII, secrets. Use English."
	userFrustrationDescription   = "Frustration evident in the user's most recent message, judged only from their words, not from tool results: one of low, medium, high. Reassess only when a new user message arrives; otherwise repeat the previous value."

	telemetryDescriptionHint = "\n\nPass telemetry.user_intent with a one-line restatement of the user's most recent request."
	// Pre-V1 hint, recognized (never emitted) so AppendTelemetryHint stays
	// idempotent on descriptions written by an older SDK build.
	telemetryDescriptionHintLegacy = "\n\nPass telemetry.intent with a one-line user intent for analytics."
)

// extractTelemetryFromArgs returns the normalized Telemetry block (if any)
// and a cleaned copy of args with the telemetry key removed. Both the V1 and
// the pre-V1 key spellings are accepted; the result is canonicalized via
// NormalizeTelemetry.
func extractTelemetryFromArgs(args map[string]any) (Telemetry, map[string]any) {
	var t Telemetry
	if args == nil {
		return t, args
	}
	raw, ok := args["telemetry"]
	if !ok {
		return t, args
	}

	switch v := raw.(type) {
	case map[string]any:
		if turn, ok := integralTurn(v["user_turn"]); ok {
			t.UserTurn = turn
		}
		// A V1 string key that is PRESENT shadows its legacy counterpart even
		// when empty — the Telemetry struct's zero value can't distinguish
		// "explicitly blank" from "absent", so the conflict must be resolved
		// here, before the map collapses into the struct. Matches the TS and
		// Python normalizers' first-string-wins rule. (user_frustration is the
		// exception: an off-spec V1 value falls through to a valid legacy one,
		// same as the other SDKs.)
		userIntent, hasUserIntent := v["user_intent"].(string)
		if hasUserIntent {
			t.UserIntent = userIntent
		} else if s, ok := v["intent"].(string); ok {
			t.Intent = s
		}
		agentThinking, hasAgentThinking := v["agent_thinking"].(string)
		if hasAgentThinking {
			t.AgentThinking = agentThinking
		} else if s, ok := v["context"].(string); ok {
			t.Context = s
		}
		if s, ok := v["user_frustration"].(string); ok {
			t.UserFrustration = s
		}
		if s, ok := v["frustration_level"].(string); ok {
			t.FrustrationLevel = s
		}
	case string:
		// Some clients flatten the block to a JSON string; tolerate it.
		var m map[string]any
		if err := json.Unmarshal([]byte(v), &m); err == nil {
			// Re-extract with the decoded block in place of the string so
			// the sibling args survive into the cleaned copy.
			merged := make(map[string]any, len(args))
			for k, av := range args {
				merged[k] = av
			}
			merged["telemetry"] = m
			return extractTelemetryFromArgs(merged)
		}
	}

	cleaned := make(map[string]any, len(args)-1)
	for k, v := range args {
		if k == "telemetry" {
			continue
		}
		cleaned[k] = v
	}
	return NormalizeTelemetry(t), cleaned
}

// integralTurn accepts a JSON number (or int) holding a 1-based integral turn
// count. Integral floats (2.0 — JSON numbers decode as float64) are accepted;
// fractional, zero, or negative values are dropped rather than coerced.
func integralTurn(value any) (int, bool) {
	switch n := value.(type) {
	case int:
		if n >= 1 {
			return n, true
		}
	case float64:
		if n >= 1 && n == math.Trunc(n) {
			return int(n), true
		}
	case json.Number:
		if f, err := n.Float64(); err == nil && f >= 1 && f == math.Trunc(f) {
			return int(f), true
		}
	}
	return 0, false
}
