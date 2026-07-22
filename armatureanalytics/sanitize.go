package armatureanalytics

import (
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

const (
	BinaryRemovedPlaceholder   = "[binary removed]"
	Base64RemovedPlaceholder   = "[base64 removed]"
	RedactionFailedPlaceholder = "[redaction failed]"
	SanitizationBudget         = 65_536
)

var base64PayloadPattern = regexp.MustCompile(`^[A-Za-z0-9+/_-]+={0,2}$`)

// embeddedBase64Pattern finds base64-alphabet runs long enough to be payloads
// inside larger strings, e.g. a blob echoed within a JSON-serialized tool
// result's text content.
var embeddedBase64Pattern = regexp.MustCompile(`[A-Za-z0-9+/_-]{512,}={0,2}`)

var jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()

// json.Number and json.RawMessage carry JSON wire semantics the reflective
// walk would otherwise corrupt: Number has Kind String and would be quoted
// like text, RawMessage is []byte and would be emitted as a byte array. They
// are matched by concrete type so the preview renders their JSON meaning.
var (
	jsonNumberType     = reflect.TypeOf(json.Number(""))
	jsonRawMessageType = reflect.TypeOf(json.RawMessage(nil))
)

type sanitizationState struct {
	remaining int
	seen      map[visit]bool
}

type visit struct {
	typ reflect.Type
	ptr uintptr
}

// SanitizeValue removes binary payloads and bounds the copied string content
// to SanitizationBudget bytes. Composite values are copied so customer-owned
// arguments and results are never mutated.
func SanitizeValue(value any) any {
	state := &sanitizationState{
		remaining: SanitizationBudget,
		seen:      make(map[visit]bool),
	}
	return state.sanitize(reflect.ValueOf(value))
}

func (s *sanitizationState) charge(units int) bool {
	if s.remaining < units {
		s.remaining = 0
		return false
	}
	s.remaining -= units
	return true
}

func (s *sanitizationState) sanitizeString(value string) string {
	// Bound all pattern work to the retainable window first: previews are
	// truncated anyway, so scanning beyond the budget is pure waste on large
	// payloads (see the redaction design: never copy an unlimited value to
	// retain a bounded preview).
	if len(value) > s.remaining {
		value, _ = truncateUTF8(value, s.remaining)
	}
	if isBase64Payload(value) {
		value = Base64RemovedPlaceholder
	} else if len(value) >= 512 {
		value = embeddedBase64Pattern.ReplaceAllString(value, Base64RemovedPlaceholder)
	}
	if len(value) <= s.remaining {
		s.remaining -= len(value)
		return value
	}
	value, _ = truncateUTF8(value, s.remaining)
	s.remaining = 0
	return value
}

// marshalPreviewCap bounds how much wire JSON the preview path is willing to
// materialize. Values estimated above the cap keep the bounded reflective
// walk so a 20 MB tool result never costs a full marshal round-trip just to
// produce a truncated preview.
const marshalPreviewCap = 4 * SanitizationBudget

// decodeJSONMarshaler renders a value that owns its JSON wire format (e.g.
// framework result types like mcp.CallToolResult) through that format instead
// of walking its raw struct fields, so previews match what actually crossed
// the wire. Oversized values, marshal failures (cycles, unsupported members),
// and non-marshaler types fall back to the reflective walk.
func decodeJSONMarshaler(value reflect.Value) (any, bool) {
	if !value.CanInterface() || !value.Type().Implements(jsonMarshalerType) {
		return nil, false
	}
	if approxSizeExceeds(value, marshalPreviewCap) {
		return nil, false
	}
	raw, err := json.Marshal(value.Interface())
	if err != nil {
		return nil, false
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, false
	}
	return decoded, true
}

// approxSizeExceeds cheaply estimates whether a value's serialized size
// exceeds the cap, without copying anything and with early exit. Depth is
// capped so cyclic values report as oversized and take the reflective walk,
// which owns cycle detection.
func approxSizeExceeds(value reflect.Value, cap int) bool {
	remaining := cap
	var walk func(v reflect.Value, depth int) bool
	walk = func(v reflect.Value, depth int) bool {
		if remaining <= 0 || depth > 64 {
			remaining = 0
			return true
		}
		if !v.IsValid() {
			return false
		}
		switch v.Kind() {
		case reflect.Interface, reflect.Pointer:
			if v.IsNil() {
				return false
			}
			return walk(v.Elem(), depth+1)
		case reflect.String:
			remaining -= v.Len() + 8
		case reflect.Slice, reflect.Array:
			if v.Kind() == reflect.Slice && v.IsNil() {
				return false
			}
			if v.Type().Elem().Kind() == reflect.Uint8 {
				remaining -= v.Len() + 8
				break
			}
			for i := 0; i < v.Len(); i++ {
				if walk(v.Index(i), depth+1) {
					return true
				}
			}
		case reflect.Map:
			if v.IsNil() {
				return false
			}
			iter := v.MapRange()
			for iter.Next() {
				if walk(iter.Key(), depth+1) || walk(iter.Value(), depth+1) {
					return true
				}
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if walk(v.Field(i), depth+1) {
					return true
				}
			}
		default:
			remaining -= 8
		}
		return remaining <= 0
	}
	return walk(value, 0)
}

func isBase64Payload(value string) bool {
	if len(value) >= 64 && strings.HasPrefix(value, "data:") && strings.Contains(value, ";base64,") {
		return true
	}
	return len(value) >= 512 && base64PayloadPattern.MatchString(value)
}

func (s *sanitizationState) sanitize(value reflect.Value) any {
	if !value.IsValid() {
		return nil
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil
		}
		return s.sanitize(value.Elem())
	}

	switch value.Kind() {
	case reflect.String:
		if value.Type() == jsonNumberType {
			// json.Number has Kind String but a numeric wire meaning.
			// Returning it verbatim keeps numeric arguments rendering
			// unquoted (50, not "50") once the preview is re-marshaled
			// (mcp-tester#1397).
			return value.Interface()
		}
		return s.sanitizeString(value.String())
	case reflect.Pointer:
		if value.IsNil() {
			return nil
		}
		if decoded, ok := decodeJSONMarshaler(value); ok {
			return s.sanitize(reflect.ValueOf(decoded))
		}
		key := visit{typ: value.Type(), ptr: value.Pointer()}
		if s.seen[key] {
			return s.sanitizeString("[circular]")
		}
		s.seen[key] = true
		defer delete(s.seen, key)
		return s.sanitize(value.Elem())
	case reflect.Map:
		if value.IsNil() {
			return nil
		}
		if value.Type().Key().Kind() != reflect.String {
			return value.Interface()
		}
		key := visit{typ: value.Type(), ptr: value.Pointer()}
		if s.seen[key] {
			return s.sanitizeString("[circular]")
		}
		s.seen[key] = true
		defer delete(s.seen, key)
		return s.sanitizeMap(value)
	case reflect.Slice:
		if value.IsNil() {
			return nil
		}
		if value.Type() == jsonRawMessageType {
			// json.RawMessage is []byte holding raw JSON; the generic slice
			// walk would emit it as a byte array (e.g. [110,117,108,108] for
			// "null"). Decode it so previews match the JSON wire format
			// (mcp-tester#1398).
			return s.sanitizeRawJSON(value.Bytes())
		}
		key := visit{typ: value.Type(), ptr: value.Pointer()}
		if key.ptr != 0 {
			if s.seen[key] {
				return s.sanitizeString("[circular]")
			}
			s.seen[key] = true
			defer delete(s.seen, key)
		}
		return s.sanitizeList(value)
	case reflect.Array:
		return s.sanitizeList(value)
	case reflect.Struct:
		if decoded, ok := decodeJSONMarshaler(value); ok {
			return s.sanitize(reflect.ValueOf(decoded))
		}
		return s.sanitizeStruct(value)
	case reflect.Bool:
		return value.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Interface()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Interface()
	case reflect.Float32, reflect.Float64:
		return value.Interface()
	default:
		return value.Interface()
	}
}

func (s *sanitizationState) sanitizeMap(value reflect.Value) map[string]any {
	keys := value.MapKeys()
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	out := make(map[string]any, len(keys))
	objectType := mapStringValue(value, "type")
	for _, mapKey := range keys {
		key := mapKey.String()
		if !s.charge(len(key) + 2) {
			break
		}
		entry := value.MapIndex(mapKey)
		if isBinaryField(key, objectType, entry) {
			out[key] = s.sanitizeString(BinaryRemovedPlaceholder)
		} else {
			out[key] = s.sanitize(entry)
		}
		if s.remaining == 0 {
			break
		}
	}
	return out
}

func (s *sanitizationState) sanitizeList(value reflect.Value) []any {
	out := make([]any, 0, value.Len())
	for i := 0; i < value.Len(); i++ {
		if !s.charge(2) {
			break
		}
		out = append(out, s.sanitize(value.Index(i)))
		if s.remaining == 0 {
			break
		}
	}
	return out
}

// sanitizeRawJSON renders embedded raw JSON (json.RawMessage) through its JSON
// meaning rather than the reflective []byte walk, then sanitizes the decoded
// value. UseNumber preserves integer precision, and the resulting json.Number
// values render unquoted via the String case. Oversized or malformed payloads
// fall back to the bounded string path.
func (s *sanitizationState) sanitizeRawJSON(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > marshalPreviewCap {
		return s.sanitizeString(string(raw))
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return s.sanitizeString(string(raw))
	}
	return s.sanitize(reflect.ValueOf(decoded))
}

func (s *sanitizationState) sanitizeStruct(value reflect.Value) map[string]any {
	typeOf := value.Type()
	objectType := structStringValue(value, "type")
	type field struct {
		name  string
		value reflect.Value
	}
	fields := make([]field, 0, value.NumField())
	for i := 0; i < value.NumField(); i++ {
		definition := typeOf.Field(i)
		if definition.PkgPath != "" { // unexported
			continue
		}
		name, omitEmpty, skip := jsonFieldName(definition)
		if skip || (omitEmpty && value.Field(i).IsZero()) {
			continue
		}
		fields = append(fields, field{name: name, value: value.Field(i)})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].name < fields[j].name })
	out := make(map[string]any, len(fields))
	for _, item := range fields {
		if !s.charge(len(item.name) + 2) {
			break
		}
		if isBinaryField(item.name, objectType, item.value) {
			out[item.name] = s.sanitizeString(BinaryRemovedPlaceholder)
		} else {
			out[item.name] = s.sanitize(item.value)
		}
		if s.remaining == 0 {
			break
		}
	}
	return out
}

func mapStringValue(value reflect.Value, key string) string {
	entry := value.MapIndex(reflect.ValueOf(key).Convert(value.Type().Key()))
	return reflectedString(entry)
}

func structStringValue(value reflect.Value, jsonName string) string {
	typeOf := value.Type()
	for i := 0; i < value.NumField(); i++ {
		name, _, skip := jsonFieldName(typeOf.Field(i))
		if !skip && name == jsonName {
			return reflectedString(value.Field(i))
		}
	}
	return ""
}

func reflectedString(value reflect.Value) string {
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return ""
		}
		value = value.Elem()
	}
	if value.IsValid() && value.Kind() == reflect.String {
		return value.String()
	}
	return ""
}

func isBinaryField(key, objectType string, value reflect.Value) bool {
	if reflectedString(value) == "" {
		return false
	}
	return key == "blob" || (key == "data" && (objectType == "image" || objectType == "audio"))
}

func jsonFieldName(field reflect.StructField) (name string, omitEmpty bool, skip bool) {
	tag := field.Tag.Get("json")
	parts := strings.Split(tag, ",")
	if len(parts) > 0 && parts[0] == "-" {
		return "", false, true
	}
	name = field.Name
	if len(parts) > 0 && parts[0] != "" {
		name = parts[0]
	}
	for _, option := range parts[1:] {
		if option == "omitempty" {
			omitEmpty = true
		}
	}
	return name, omitEmpty, false
}
