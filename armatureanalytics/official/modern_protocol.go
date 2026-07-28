package official

// MCP 2026-07-28 ("modern era") support. The stateless protocol removed the
// initialize handshake and the Mcp-Session-Id header: every request carries
// protocolVersion + clientCapabilities (and optionally clientInfo) in fixed
// `_meta` slots, and W3C trace context (traceparent / tracestate / baggage)
// travels in fixed `_meta` slots as well. go-sdk v1.7.0 parses the identity
// triple (mcp.MetaKey* constants, typed ServerRequest accessors) but ships no
// helpers for the trace slots, so those are read from the raw `_meta` map.

import (
	"encoding/json"
	"net/url"
	"reflect"
	"strings"

	armatureanalytics "github.com/armature-tech/mcp-analytics-go/armatureanalytics"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// modernProtocolVersion is the first stateless MCP protocol version.
const modernProtocolVersion = "2026-07-28"

// Fixed `_meta` slots for W3C trace context (MCP 2026-07-28). The SDK exposes
// no constants for these.
const (
	metaKeyBaggage = "baggage"
)

// baggageConversationKey is the OpenTelemetry GenAI baggage member naming the
// logical conversation — the strongest session signal the modern protocol
// offers, since transport-level session IDs no longer exist.
const baggageConversationKey = "gen_ai.conversation.id"

// requestMeta returns the raw `_meta` map from the request's params. Nil-safe
// against nil requests and absent or typed-nil params.
func requestMeta(req mcp.Request) map[string]any {
	if req == nil {
		return nil
	}
	params := req.GetParams()
	if params == nil {
		return nil
	}
	value := reflect.ValueOf(params)
	if (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) && value.IsNil() {
		return nil
	}
	return params.GetMeta()
}

// isModernRequest reports whether the request self-identifies as 2026-07-28
// or later through the per-request `_meta` protocolVersion slot. Mirrors the
// SDK's own validateRequestMeta detection.
func isModernRequest(meta map[string]any) bool {
	version, ok := meta[mcp.MetaKeyProtocolVersion].(string)
	return ok && version >= modernProtocolVersion
}

// perRequestClientInfo captures client identity for a single request. Where
// the concrete request type is known (tools/call), the SDK's typed accessors
// are used: they read the 2026-07-28 per-request `_meta` slots first and fall
// back to the session's InitializeParams for legacy-era traffic (which the
// SDK also synthesizes from `_meta` on modern sessions). Other request shapes
// decode the raw `_meta` map with the mcp.MetaKey* constants. Returns nil
// when the request carries no client identity at all.
func perRequestClientInfo(req mcp.Request) *armatureanalytics.ClientInfo {
	if call, ok := req.(*mcp.CallToolRequest); ok && call != nil {
		return clientInfoFromParts(call.ProtocolVersion(), call.ClientInfo(), capabilitiesToMap(call.ClientCapabilities()))
	}
	meta := requestMeta(req)
	if len(meta) == 0 {
		return nil
	}
	version, _ := meta[mcp.MetaKeyProtocolVersion].(string)
	return clientInfoFromParts(version, decodeImplementation(meta[mcp.MetaKeyClientInfo]), decodeToMap(meta[mcp.MetaKeyClientCapabilities]))
}

func clientInfoFromParts(protocolVersion string, impl *mcp.Implementation, capabilities map[string]any) *armatureanalytics.ClientInfo {
	info := &armatureanalytics.ClientInfo{ProtocolVersion: protocolVersion, Capabilities: capabilities}
	if impl != nil {
		info.Name = impl.Name
		info.Version = impl.Version
	}
	if info.Name == "" && info.Version == "" && info.ProtocolVersion == "" && len(info.Capabilities) == 0 {
		// clientInfo is OPTIONAL per request in 2026-07-28; an absent identity
		// yields a nil ClientInfo (an "unknown" client downstream), never an
		// error.
		return nil
	}
	return info
}

// decodeImplementation accepts either an in-process *mcp.Implementation or
// the generic JSON map produced by wire transit.
func decodeImplementation(value any) *mcp.Implementation {
	switch v := value.(type) {
	case nil:
		return nil
	case *mcp.Implementation:
		return v
	case mcp.Implementation:
		return &v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		var impl mcp.Implementation
		if err := json.Unmarshal(data, &impl); err != nil {
			return nil
		}
		return &impl
	}
}

// decodeToMap renders a `_meta` value (typed struct or wire map) as a generic
// map for the ClientInfo.Capabilities field.
func decodeToMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	if m, ok := value.(map[string]any); ok {
		return m
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil
	}
	return decoded
}

func capabilitiesToMap(capabilities *mcp.ClientCapabilities) map[string]any {
	if capabilities == nil {
		return nil
	}
	return decodeToMap(capabilities)
}

// conversationIDFromMeta extracts `gen_ai.conversation.id` from the request's
// `_meta` "baggage" slot.
func conversationIDFromMeta(meta map[string]any) string {
	baggage, _ := meta[metaKeyBaggage].(string)
	if baggage == "" {
		return ""
	}
	return baggageMember(baggage, baggageConversationKey)
}

// baggageMember returns one member's value from a W3C Baggage header value:
// comma-separated `key=value` members, each optionally carrying
// semicolon-delimited properties, with percent-encoded values.
func baggageMember(baggage, key string) string {
	for _, member := range strings.Split(baggage, ",") {
		if i := strings.IndexByte(member, ';'); i >= 0 {
			member = member[:i]
		}
		name, value, ok := strings.Cut(member, "=")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		value = strings.TrimSpace(value)
		// PathUnescape decodes the percent-encoding W3C baggage uses without
		// rewriting '+' the way query unescaping would.
		if decoded, err := url.PathUnescape(value); err == nil {
			return decoded
		}
		return value
	}
	return ""
}

// copyRequestMeta shallow-copies `_meta` so the captured snapshot cannot be
// mutated by handlers before the background privacy queue serializes it.
func copyRequestMeta(meta map[string]any) map[string]any {
	if len(meta) == 0 {
		return nil
	}
	out := make(map[string]any, len(meta))
	for k, v := range meta {
		out[k] = v
	}
	return out
}

// snapshotToolResult copies a *mcp.CallToolResult envelope and its Meta map.
// v1.7.0's response pipeline mutates result._meta (annotateServerInfo) after
// receiving middleware has returned, so the analytics queue must never hold
// the live value.
func snapshotToolResult(result mcp.Result) mcp.Result {
	toolResult, ok := result.(*mcp.CallToolResult)
	if !ok || toolResult == nil {
		return result
	}
	snapshot := *toolResult
	snapshot.Meta = copyRequestMeta(toolResult.Meta)
	return &snapshot
}
