package armatureanalytics

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Tools whose input schema declares its own top-level `telemetry` property are
// customer-owned (TELEMETRY-CONTRACT.md, mode "owned"): the SDK must not
// inject, strip, or interpret that field — including in the recorder hooks,
// which otherwise can't see tool schemas. Registration paths record ownership
// here; hooks consult it per call.
//
// Keyed by tool name, process-wide, last registration wins: a successful
// decoration clears any stale owned mark for that name, so re-registering a
// renamed-field tool (or replacing a recorder) recovers telemetry capture.
// Known limitation: two servers in one process registering same-named tools
// with DIFFERENT ownership will share the most recent registration's
// semantics — a same-named tool with two contracts in one process is already
// ambiguous at the analytics level, since hooks only see the tool name.
var ownedTelemetryTools sync.Map // tool name → struct{}

// MarkTelemetryOwnedTool records that the named tool owns its `telemetry`
// input field, logging the contract collision warning once per name. Adapter
// packages call this when their registration path detects the collision.
func MarkTelemetryOwnedTool(name string) {
	if _, already := ownedTelemetryTools.LoadOrStore(name, struct{}{}); already {
		return
	}
	log.Printf(
		"[mcp-analytics] Tool %q already declares a top-level \"telemetry\" input field; leaving the tool untouched and not collecting Armature telemetry for it. Rename the field or configure telemetryFieldMap to export it explicitly.",
		name,
	)
}

// IsTelemetryOwnedTool reports whether the named tool was recorded as owning
// its `telemetry` input field.
func IsTelemetryOwnedTool(name string) bool {
	_, ok := ownedTelemetryTools.Load(name)
	return ok
}

// UnmarkTelemetryOwnedTool clears a recorded ownership mark. Registration
// paths call this when they successfully decorate a tool, so the registry
// always reflects the most recent registration of a name and a stale mark
// cannot outlive a rename or a recorder replacement.
func UnmarkTelemetryOwnedTool(name string) {
	ownedTelemetryTools.Delete(name)
}

// resetOwnedTelemetryToolsForTests clears the ownership registry.
func resetOwnedTelemetryToolsForTests() {
	ownedTelemetryTools.Range(func(key, _ any) bool {
		ownedTelemetryTools.Delete(key)
		return true
	})
}

// Telemetry holds the optional LLM-supplied analytics fields injected into
// every wrapped tool's input schema. When AddTool / WrapHandler are used the
// LLM may populate any subset of these; the handler never sees them — they're
// stripped from the request args and attached to the context for the
// recorder's hooks to pick up.
//
// CallPurpose is the public action summary accepted on input. Normalized
// values keep the historical event field names. Older spellings are accepted on
// input (clients holding a cached pre-V1 tool schema, callers passing a
// Telemetry straight into WithTelemetry) and are normalized onto the V1
// fields — with legacy mirrors filled back in — by NormalizeTelemetry before
// any event is built. UserTurn, UserFrustration and FrustrationLevel remain
// only for source compatibility; they are ignored and never exported.
type Telemetry struct {
	// Deprecated: cached clients may still send this field. It is ignored.
	UserTurn    int    `json:"user_turn,omitempty"`
	UserIntent  string `json:"user_intent,omitempty"`
	CallPurpose string `json:"call_purpose,omitempty"`
	// Deprecated: accepted for cached clients. Use CallPurpose for new code.
	AgentThinking string `json:"agent_thinking,omitempty"`
	// Deprecated: no longer advertised, read or exported. A user_frustration
	// value sent by a cached client is stripped with the telemetry argument
	// and dropped. NormalizeTelemetry clears this field; the SDK never
	// populates it.
	UserFrustration string `json:"-"`
	// Deprecated: pre-V1 spelling of UserIntent; still accepted.
	Intent string `json:"intent,omitempty"`
	// Deprecated: pre-V1 spelling of AgentThinking; still accepted.
	Context string `json:"context,omitempty"`
	// Deprecated: pre-V1 spelling of UserFrustration. Ignored like it and
	// never populated.
	FrustrationLevel string `json:"-"`
	// Preserves an explicitly blank call_purpose from JSON or tool arguments.
	callPurposePresent bool
}

// UnmarshalJSON preserves call_purpose presence so an explicit empty string
// overrides cached agent_thinking and context values. Other call_purpose
// types are ignored so valid legacy fields remain available.
func (t *Telemetry) UnmarshalJSON(data []byte) error {
	type plain Telemetry
	// Shadow the new input alias with raw JSON. The existing fields still use
	// their typed decoder, preserving their previous validation behavior.
	var decoded struct {
		plain
		CallPurpose json.RawMessage `json:"call_purpose"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	raw := strings.TrimSpace(string(decoded.CallPurpose))
	if strings.HasPrefix(raw, `"`) {
		if err := json.Unmarshal(decoded.CallPurpose, &decoded.plain.CallPurpose); err != nil {
			return err
		}
		decoded.callPurposePresent = true
	}
	*t = Telemetry(decoded.plain)
	return nil
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

// ExtractTelemetryFromArguments returns the normalized telemetry block and
// args with the top-level telemetry property removed. When telemetry is absent,
// the original map may be returned. Adapter packages use this to share parsing
// and legacy-field behavior.
func ExtractTelemetryFromArguments(args map[string]any) (Telemetry, map[string]any) {
	return extractTelemetryFromArgs(args)
}

// TelemetryInputSchema returns a fresh JSON Schema fragment for the optional
// telemetry object injected by the framework adapters.
func TelemetryInputSchema() map[string]any {
	return telemetrySchemaObject()
}

// NormalizeTelemetry moves CallPurpose into the historical AgentThinking
// storage field and fills legacy mirrors. CallPurpose takes precedence over
// AgentThinking and Context. Explicit empty JSON or tool argument values also
// win. UserTurn, UserFrustration and FrustrationLevel are dropped. Matches the
// TS and Python normalizers.
func NormalizeTelemetry(t Telemetry) Telemetry {
	out := Telemetry{}
	out.UserIntent = firstNonEmpty(t.UserIntent, t.Intent)
	out.callPurposePresent = t.callPurposePresent && t.CallPurpose == ""
	if t.callPurposePresent || t.CallPurpose != "" {
		out.AgentThinking = t.CallPurpose
	} else {
		out.AgentThinking = firstNonEmpty(t.AgentThinking, t.Context)
	}
	// Legacy mirrors so a not-yet-updated ingest keeps reading events built
	// from this value.
	out.Intent = out.UserIntent
	out.Context = out.AgentThinking
	return out
}

// applyTelemetryFieldMap implements the opt-in export of customer-owned
// argument fields (gap #11, TELEMETRY-CONTRACT.md): it reads — never strips —
// the mapped top-level argument properties and fills any telemetry field the
// call didn't already provide explicitly. Values are validated with the same
// rules as NormalizeTelemetry, so a wrong-typed customer field is ignored
// rather than exported as garbage. A user_frustration mapping is accepted and
// ignored.
func applyTelemetryFieldMap(t Telemetry, args any, fieldMap map[string]string) Telemetry {
	if len(fieldMap) == 0 {
		return t
	}
	m, ok := args.(map[string]any)
	if !ok {
		return t
	}
	argString := func(field string) string {
		key := fieldMap[field]
		if key == "" {
			return ""
		}
		s, _ := m[key].(string)
		return s
	}
	if t.UserIntent == "" && t.Intent == "" {
		t.UserIntent = argString("user_intent")
	}
	if !t.callPurposePresent && t.CallPurpose == "" && t.AgentThinking == "" && t.Context == "" {
		if key := fieldMap["call_purpose"]; key != "" {
			if value, ok := m[key].(string); ok && value != "" {
				t.CallPurpose = value
				t.callPurposePresent = true
			}
		}
		if !t.callPurposePresent {
			t.AgentThinking = argString("agent_thinking")
		}
	}
	return t
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// InstrumentTool registers a tool on s and instruments it for Armature
// analytics telemetry capture. Servers created by NewMCPServerWithConfig
// automatically supply their capture policy; standalone servers default to
// capture enabled and can use InstrumentToolWithConfig explicitly. It
// decorates the tool's input schema with an optional `telemetry` object
// (user_intent / call_purpose) and wraps the handler so that the telemetry
// arguments are stripped before the handler runs but kept on the request
// context for the recorder's hooks to read. It adds no text to the tool
// description; an exact SDK hint suffix from an earlier release is removed.
//
// Mirrors the TS SDK's instrumentMcpServerTools, applied one tool at a
// time. InstrumentTool is purely additive on top of Recorder.Hooks(): the
// hook chain must still be installed via server.WithHooks(rec.Hooks()) for
// events to reach Armature. Tools registered with the plain server.AddTool
// still emit events, just without intent metadata.
//
// Tools whose schema already declares a top-level `telemetry` input are
// registered untouched (no decoration, no description change, no handler
// wrap): stripping that argument would swallow a real input the tool
// advertises. The same applies when a RawInputSchema cannot be parsed and
// extended.
func InstrumentTool(s *server.MCPServer, tool mcp.Tool, handler server.ToolHandlerFunc) {
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
func InstrumentToolWithConfig(cfg Config, s *server.MCPServer, tool mcp.Tool, handler server.ToolHandlerFunc) {
	decorated, ok := decorateToolSchema(tool)
	if !ok {
		// Owned telemetry field (recorded by decorateToolSchema) or an
		// unparseable raw schema: register untouched, never strip.
		s.AddTool(tool, handler)
		return
	}
	if !cfg.captureEnabled() {
		s.AddTool(tool, WrapHandler(handler))
		return
	}
	decorated.Description = StripTelemetryHint(decorated.Description)
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

// StripTelemetryHint returns description without the task-context hint an
// earlier release of this SDK appended to it, including that hint followed by
// a request_capability sentence. The SDK adds no text to tool descriptions;
// InstrumentTool applies this automatically, and custom registration paths
// can call it so descriptions registered through an older wrapper come out
// clean. Only an exact SDK suffix is removed: customer prose that quotes a
// hint is preserved, and the result is stable under repeated calls. A
// description that consisted only of an SDK hint becomes "".
func StripTelemetryHint(description string) string {
	return stripLegacyTelemetryHint(description)
}

// AppendTelemetryHint is StripTelemetryHint.
//
// Deprecated: the SDK no longer appends a hint to tool descriptions. This
// function only removes hints appended by earlier releases. Use
// StripTelemetryHint.
func AppendTelemetryHint(description string) string {
	return stripLegacyTelemetryHint(description)
}

// HintOptions tuned AppendTelemetryHintWithOptions.
//
// Deprecated: no hint is appended, so every option is ignored.
type HintOptions struct {
	// Deprecated: ignored. No tool description mentions send_feedback.
	RequestCapability bool
	// Deprecated: ignored. No description-length notice is logged.
	ToolName string
	// Deprecated: ignored. No description-length notice is logged.
	LogLevel string
}

// AppendTelemetryHintWithOptions is StripTelemetryHint; opts are ignored.
//
// Deprecated: the SDK no longer appends a hint to tool descriptions. Use
// StripTelemetryHint.
func AppendTelemetryHintWithOptions(description string, _ HintOptions) string {
	return stripLegacyTelemetryHint(description)
}

// MaxToolDescriptionLength was the description length the appended hint was
// kept within, in UTF-8 bytes.
//
// Deprecated: the SDK no longer appends text to tool descriptions, so it
// enforces no length budget.
const MaxToolDescriptionLength = 1024

// stripLegacyTelemetryHint removes only a known SDK suffix. Identical words
// embedded in customer prose are left untouched.
func stripLegacyTelemetryHint(description string) string {
	for {
		stripped := false
		for _, suffix := range legacyTelemetrySuffixes {
			if description == strings.TrimLeft(suffix, "\n") {
				return ""
			}
			if strings.HasSuffix(description, suffix) {
				description = strings.TrimSuffix(description, suffix)
				stripped = true
				break
			}
		}
		if !stripped {
			return description
		}
	}
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
// under `properties`, is itself an object with user_intent / call_purpose, and
// is NEVER added to the schema's `required` array. The Required list inside
// the telemetry object is also empty by design — user_intent is a soft nudge.
// The tool description is left as is; see StripTelemetryHint.
func DecorateInputSchemaWithTelemetry(tool mcp.Tool) (mcp.Tool, bool) {
	return decorateToolSchema(tool)
}

func decorateToolSchema(tool mcp.Tool) (mcp.Tool, bool) {
	// Prefer structured InputSchema; fall back to RawInputSchema for callers
	// who hand-built the JSON.
	if tool.RawInputSchema != nil && tool.InputSchema.Type == "" {
		raw, ok, owned := injectTelemetryIntoRawSchema(tool.RawInputSchema)
		if owned {
			MarkTelemetryOwnedTool(tool.Name)
		}
		if !ok {
			return tool, false
		}
		tool.RawInputSchema = raw
		UnmarkTelemetryOwnedTool(tool.Name)
		return tool, true
	}

	src := tool.InputSchema.Properties
	if _, exists := src["telemetry"]; exists {
		// The tool declares its own telemetry input. Leave the schema alone,
		// tell the caller not to strip that argument, and record ownership so
		// the recorder hooks never interpret the customer's value either.
		MarkTelemetryOwnedTool(tool.Name)
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
	// Last registration wins: a name previously marked owned that now
	// decorates cleanly (field renamed, recorder replaced) captures again.
	UnmarkTelemetryOwnedTool(tool.Name)
	return tool, true
}

// injectTelemetryIntoRawSchema parses raw, adds telemetry under properties,
// and re-marshals. ok is false if raw is not a JSON object we can extend or
// the schema already declares telemetry; owned distinguishes the latter so
// the caller can record customer ownership of the field.
func injectTelemetryIntoRawSchema(raw json.RawMessage) (_ json.RawMessage, ok bool, owned bool) {
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, false, false
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		props = make(map[string]any, 1)
	}
	if _, exists := props["telemetry"]; exists {
		// Pre-existing telemetry input; the caller must not strip it.
		return nil, false, true
	}
	props["telemetry"] = telemetrySchemaObject()
	schema["properties"] = props
	if _, hasType := schema["type"]; !hasType {
		schema["type"] = "object"
	}
	out, err := json.Marshal(schema)
	if err != nil {
		return nil, false, false
	}
	return out, true, false
}

// telemetrySchemaObject returns the JSON Schema fragment describing the
// optional telemetry block. All sub-fields are optional.
func telemetrySchemaObject() map[string]any {
	return map[string]any{
		"type":        "object",
		"description": telemetryPropertyDescription,
		"properties": map[string]any{
			"user_intent": map[string]any{
				"type":        "string",
				"description": userIntentDescription,
			},
			"call_purpose": map[string]any{
				"type":        "string",
				"description": callPurposeDescription,
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
	telemetryPropertyDescription = "Optional task context for usage analytics, based on the visible user request and the action performed by this tool."
	userIntentDescription        = "Generalized one-sentence summary of the task stated in the user's latest message. Describe actions and generic roles only. Replace all tool argument values with generic terms, including names, contacts, IDs, credentials, document titles, team names and filters. For example, 'List employees in the selected team.' Include only on the first tool call after each new user message; omit on later calls in the same turn. Use English."
	callPurposeDescription       = "Short public description of the action this tool performs toward the user's stated goal. Base it only on the visible request, the tool's function and its inputs. Use English. Omit names, contact details, identifiers, credentials and argument values. Generalize document titles, team names and filter values (for example, 'the selected team')."
)

// Description suffixes earlier releases appended. They are recognized only so
// StripTelemetryHint can remove them; the SDK never emits them.
const (
	telemetryDescriptionHintCallPurpose  = "\n\nInclude telemetry.call_purpose with a short description of this action. Include telemetry.user_intent and telemetry.user_frustration only on the first tool call after each new user message."
	telemetryDescriptionHintReasoning    = "\n\nOn every call, pass telemetry.agent_thinking with your reasoning for this specific call. Pass telemetry.user_intent only on the first tool call after a new user message."
	telemetrySentenceHintReasoning       = "\n\nPass telemetry.agent_thinking on every call, telemetry.user_intent on the first call after each user message."
	telemetryDescriptionHintRepeatIntent = "\n\nPass telemetry.user_intent with a one-line restatement of the user's most recent request, and telemetry.agent_thinking with your reasoning for making this specific call."
	telemetryDescriptionHintV1           = "\n\nPass telemetry.user_intent with a one-line restatement of the user's most recent request."
	telemetryDescriptionHintLegacy       = "\n\nPass telemetry.intent with a one-line user intent for analytics."
)

// Capability sentences earlier releases appended after a hint, removed with
// it. The first is the last one shipped.
var legacyRequestCapabilitySentences = []string{
	"Call request_capability before you tell the user something can't be done here or has to be done elsewhere.",
	"If no tool can do what the user asks, call request_capability.",
}

var legacyTelemetryHints = []string{
	telemetryDescriptionHintCallPurpose,
	telemetryDescriptionHintReasoning,
	telemetrySentenceHintReasoning,
	telemetryDescriptionHintRepeatIntent,
	telemetryDescriptionHintV1,
	telemetryDescriptionHintLegacy,
}

// legacyTelemetrySuffixes lists every SDK suffix to remove: each hint followed
// by each capability sentence, then the bare hint.
var legacyTelemetrySuffixes = func() []string {
	var suffixes []string
	for _, hint := range legacyTelemetryHints {
		for _, sentence := range legacyRequestCapabilitySentences {
			suffixes = append(suffixes, hint+" "+sentence)
		}
		suffixes = append(suffixes, hint)
	}
	return suffixes
}()

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
		// user_turn, user_frustration and frustration_level from cached
		// schemas are intentionally ignored: they are stripped with the rest
		// of the telemetry argument and never exported.
		// A V1 string key that is PRESENT shadows its legacy counterpart even
		// when empty — the Telemetry struct's zero value can't distinguish
		// "explicitly blank" from "absent", so the conflict must be resolved
		// here, before the map collapses into the struct. Matches the TS and
		// Python normalizers' first-string-wins rule.
		userIntent, hasUserIntent := v["user_intent"].(string)
		if hasUserIntent {
			t.UserIntent = userIntent
		} else if s, ok := v["intent"].(string); ok {
			t.Intent = s
		}
		callPurpose, hasCallPurpose := v["call_purpose"].(string)
		agentThinking, hasAgentThinking := v["agent_thinking"].(string)
		if hasCallPurpose {
			t.CallPurpose = callPurpose
			t.callPurposePresent = true
		} else if hasAgentThinking {
			t.AgentThinking = agentThinking
		} else if s, ok := v["context"].(string); ok {
			t.Context = s
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
