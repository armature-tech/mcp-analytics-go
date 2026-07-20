package armatureanalytics

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// SecretPatternRule is one ordered built-in high-confidence secret rule.
type SecretPatternRule struct {
	ID          string
	Pattern     *regexp.Regexp
	Replacement string
}

// SecretPatternRules follows the cross-SDK telemetry contract order. The
// expressions use Go's RE2 engine and intentionally avoid lookarounds and
// backreferences.
var SecretPatternRules = []SecretPatternRule{
	{ID: "pem", Pattern: regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(?s:.*?)-----END [A-Z0-9 ]*PRIVATE KEY-----`), Replacement: "[redacted:pem]"},
	{ID: "sensitive-kv", Pattern: regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|api[_-]?key|access[_-]?token|refresh[_-]?token|client[_-]?secret|private[_-]?key|authorization)([=:])([^\s"'\x60,;&]{4,})`), Replacement: `${1}${2}[redacted:sensitive-kv]`},
	{ID: "aws-access-key-id", Pattern: regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA|AGPA|AIDA|AIPA|ANPA|ANVA|AROA)[A-Z0-9]{16}\b`), Replacement: "[redacted:aws-access-key-id]"},
	{ID: "github-token", Pattern: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{22,255})\b`), Replacement: "[redacted:github-token]"},
	{ID: "google-api-key", Pattern: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), Replacement: "[redacted:google-api-key]"},
	{ID: "slack-token", Pattern: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`), Replacement: "[redacted:slack-token]"},
	{ID: "stripe-key", Pattern: regexp.MustCompile(`\b[rs]k_(?:live|test)_[A-Za-z0-9]{16,}\b`), Replacement: "[redacted:stripe-key]"},
	{ID: "anthropic-api-key", Pattern: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{16,}\b`), Replacement: "[redacted:anthropic-api-key]"},
	{ID: "openai-api-key", Pattern: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`), Replacement: "[redacted:openai-api-key]"},
	{ID: "jwt", Pattern: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{10,}\b`), Replacement: "[redacted:jwt]"},
	{ID: "connection-string", Pattern: regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+):([^\s@]+)@`), Replacement: `${1}:[redacted:connection-string]@`},
	{ID: "bearer", Pattern: regexp.MustCompile(`\b[Bb]earer +[A-Za-z0-9._~+/=-]{16,}`), Replacement: "Bearer [redacted:bearer]"},
	{ID: "basic", Pattern: regexp.MustCompile(`\b[Bb]asic +[A-Za-z0-9+/=]{16,}`), Replacement: "Basic [redacted:basic]"},
}

var sensitiveFieldNames = map[string]struct{}{
	"password": {}, "passwd": {}, "pwd": {}, "secret": {}, "apikey": {},
	"accesskey": {}, "secretkey": {}, "secretaccesskey": {}, "token": {},
	"accesstoken": {}, "refreshtoken": {}, "idtoken": {}, "sessiontoken": {},
	"authorization": {}, "auth": {}, "clientsecret": {}, "privatekey": {},
	"credential": {}, "credentials": {}, "connectionstring": {},
	"databaseurl": {}, "dsn": {},
}

// NormalizeSensitiveFieldName lowercases a key and removes underscores and
// hyphens before matching the contract's sensitive-field set.
func NormalizeSensitiveFieldName(key string) string {
	key = strings.ToLower(key)
	return strings.NewReplacer("_", "", "-", "").Replace(key)
}

// RedactSecretsInString applies all built-in value rules in contract order.
func RedactSecretsInString(value string) string {
	for _, rule := range SecretPatternRules {
		value = rule.Pattern.ReplaceAllString(value, rule.Replacement)
	}
	return value
}

// RedactSecretsInValue recursively redacts strings and sensitive string-valued
// fields without mutating the supplied value.
func RedactSecretsInValue(value any) any {
	return redactSecrets(reflect.ValueOf(value), make(map[visit]bool))
}

func redactSecrets(value reflect.Value, seen map[visit]bool) any {
	if !value.IsValid() {
		return nil
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil
		}
		return redactSecrets(value.Elem(), seen)
	}
	switch value.Kind() {
	case reflect.String:
		return RedactSecretsInString(value.String())
	case reflect.Pointer:
		if value.IsNil() {
			return nil
		}
		key := visit{typ: value.Type(), ptr: value.Pointer()}
		if seen[key] {
			return "[circular]"
		}
		seen[key] = true
		defer delete(seen, key)
		return redactSecrets(value.Elem(), seen)
	case reflect.Map:
		if value.IsNil() || value.Type().Key().Kind() != reflect.String {
			return value.Interface()
		}
		key := visit{typ: value.Type(), ptr: value.Pointer()}
		if seen[key] {
			return "[circular]"
		}
		seen[key] = true
		defer delete(seen, key)
		keys := value.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		out := make(map[string]any, len(keys))
		for _, mapKey := range keys {
			name := mapKey.String()
			entry := value.MapIndex(mapKey)
			if _, sensitive := sensitiveFieldNames[NormalizeSensitiveFieldName(name)]; sensitive && reflectedString(entry) != "" {
				out[name] = "[redacted:sensitive-field]"
			} else if _, sensitive := sensitiveFieldNames[NormalizeSensitiveFieldName(name)]; sensitive && isReflectedEmptyString(entry) {
				out[name] = "[redacted:sensitive-field]"
			} else {
				out[name] = redactSecrets(entry, seen)
			}
		}
		return out
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			return nil
		}
		if value.Kind() == reflect.Slice && value.Pointer() != 0 {
			key := visit{typ: value.Type(), ptr: value.Pointer()}
			if seen[key] {
				return "[circular]"
			}
			seen[key] = true
			defer delete(seen, key)
		}
		out := make([]any, 0, value.Len())
		for i := 0; i < value.Len(); i++ {
			out = append(out, redactSecrets(value.Index(i), seen))
		}
		return out
	case reflect.Struct:
		out := make(map[string]any)
		typeOf := value.Type()
		for i := 0; i < value.NumField(); i++ {
			definition := typeOf.Field(i)
			if definition.PkgPath != "" {
				continue
			}
			name, omitEmpty, skip := jsonFieldName(definition)
			if skip || (omitEmpty && value.Field(i).IsZero()) {
				continue
			}
			entry := value.Field(i)
			if _, sensitive := sensitiveFieldNames[NormalizeSensitiveFieldName(name)]; sensitive && isReflectedString(entry) {
				out[name] = "[redacted:sensitive-field]"
			} else {
				out[name] = redactSecrets(entry, seen)
			}
		}
		return out
	default:
		return value.Interface()
	}
}

func isReflectedString(value reflect.Value) bool {
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return false
		}
		value = value.Elem()
	}
	return value.IsValid() && value.Kind() == reflect.String
}

func isReflectedEmptyString(value reflect.Value) bool {
	return isReflectedString(value) && reflectedString(value) == ""
}
