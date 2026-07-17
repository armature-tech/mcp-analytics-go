package armatureanalytics

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
)

// Placeholder strings are part of the cross-SDK contract
// (packages/TELEMETRY-CONTRACT.md in the Armature monorepo) — golden tests in
// all three SDKs assert them byte-for-byte.
const (
	BinaryRemovedPlaceholder   = "[binary removed]"
	Base64RemovedPlaceholder   = "[base64 removed]"
	RedactionFailedPlaceholder = "[redaction failed]"
)

// A data: URI with a base64 payload is binary at any plausible size; plain
// strings need the higher bar (length + strict charset) so prose, ids, and
// hashes below half a KB pass through untouched. Both thresholds are contract
// values — keep in sync with the TypeScript and Python SDKs.
const (
	dataURIMinChars = 64
	base64MinChars  = 512
)

// Strict charset on purpose: no whitespace, so long prose (letters + spaces)
// never matches. Covers standard base64 and base64url, with optional padding.
var base64Re = regexp.MustCompile(`^[A-Za-z0-9+/_-]+={0,2}$`)

func isBase64Payload(value string) bool {
	if len(value) >= dataURIMinChars && strings.HasPrefix(value, "data:") && strings.Contains(value, ";base64,") {
		return true
	}
	return len(value) >= base64MinChars && base64Re.MatchString(value)
}

// SanitizeValue recursively strips binary and base64 payloads from a decoded
// JSON value (map[string]any / []any / string) before it is serialized into
// previews (gap #1). MCP image/audio content blocks lose their `data`,
// resource blobs lose their `blob`, and long base64 strings are replaced
// wholesale. The input is never mutated; a sanitized copy is returned.
// Cycle-safe: a self-referential container sanitizes to "[circular]" instead
// of overflowing the stack, matching the TS and Python SDKs.
func SanitizeValue(value any) any {
	return sanitizeValueSeen(value, make(map[uintptr]struct{}))
}

// sanitizeValueSeen tracks the container pointers on the CURRENT descent path
// (entries are removed on the way back up), so shared-but-acyclic values are
// each sanitized while true cycles are cut. Empty containers are not tracked:
// zero-length allocations can share the runtime's zerobase pointer, and an
// empty container cannot participate in a cycle anyway.
func sanitizeValueSeen(value any, seen map[uintptr]struct{}) any {
	switch v := value.(type) {
	case string:
		if isBase64Payload(v) {
			return Base64RemovedPlaceholder
		}
		return v
	case []any:
		if len(v) > 0 {
			ptr := reflect.ValueOf(v).Pointer()
			if _, cycling := seen[ptr]; cycling {
				return "[circular]"
			}
			seen[ptr] = struct{}{}
			defer delete(seen, ptr)
		}
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = sanitizeValueSeen(item, seen)
		}
		return out
	case map[string]any:
		if len(v) > 0 {
			ptr := reflect.ValueOf(v).Pointer()
			if _, cycling := seen[ptr]; cycling {
				return "[circular]"
			}
			seen[ptr] = struct{}{}
			defer delete(seen, ptr)
		}
		blockType, _ := v["type"].(string)
		out := make(map[string]any, len(v))
		for key, entry := range v {
			if key == "data" && (blockType == "image" || blockType == "audio") {
				if _, isString := entry.(string); isString {
					out[key] = BinaryRemovedPlaceholder
					continue
				}
			}
			if key == "blob" {
				if _, isString := entry.(string); isString {
					out[key] = BinaryRemovedPlaceholder
					continue
				}
			}
			out[key] = sanitizeValueSeen(entry, seen)
		}
		return out
	default:
		return value
	}
}

// toGenericJSON round-trips value through JSON so the sanitizer sees plain
// maps/slices/strings regardless of the concrete Go type the integration
// handed us (mcp.CallToolResult structs, json.RawMessage, typed inputs).
func toGenericJSON(value any) (any, bool) {
	if value == nil {
		return nil, true
	}
	switch value.(type) {
	case map[string]any, []any, string, bool, float64, int, int64:
		// Already generic enough for SanitizeValue; strings/numbers pass
		// through it unchanged anyway.
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		return nil, false
	}
	return generic, true
}

// prepareForPreview implements the contract pipeline (TELEMETRY-CONTRACT.md):
// sanitize → customer redact, failing closed. A redact hook that panics
// replaces the whole payload with the placeholder rather than shipping
// unredacted data; the event itself still ships.
func prepareForPreview(value any, redact func(any) any) any {
	generic, ok := toGenericJSON(value)
	if !ok {
		// Unserializable values produce no preview content anyway; hand the
		// original through so stringifyPreview reports it consistently.
		return value
	}
	sanitized := SanitizeValue(generic)
	if redact == nil {
		return sanitized
	}
	return safeRedact(redact, sanitized)
}

func safeRedact(redact func(any) any, value any) (out any) {
	defer func() {
		if recover() != nil {
			out = RedactionFailedPlaceholder
		}
	}()
	return redact(value)
}
