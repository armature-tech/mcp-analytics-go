package armatureanalytics

import (
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
	if isBase64Payload(value) {
		value = Base64RemovedPlaceholder
	}
	if len(value) <= s.remaining {
		s.remaining -= len(value)
		return value
	}
	value, _ = truncateUTF8(value, s.remaining)
	s.remaining = 0
	return value
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
		return s.sanitizeString(value.String())
	case reflect.Pointer:
		if value.IsNil() {
			return nil
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
