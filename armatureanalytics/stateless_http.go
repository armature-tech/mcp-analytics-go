package armatureanalytics

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// StatelessHTTPInput is one parsed stateless MCP HTTP request. Body accepts a
// decoded JSON object/array, json.RawMessage, or raw []byte.
type StatelessHTTPInput struct {
	Body    any
	Headers http.Header
}

// StatelessHTTPSession is the stable identity resolved for one stateless MCP
// HTTP request. Its wire format matches the TypeScript and Python SDKs.
type StatelessHTTPSession struct {
	SessionID    string
	ClientInfo   *ClientInfo
	IsInitialize bool
}

// SessionIDGenerator returns a transport generator on initialize requests and
// nil on later requests. Use it with the framework's per-request transport.
func (s StatelessHTTPSession) SessionIDGenerator() func() string {
	if !s.IsInitialize {
		return nil
	}
	return func() string { return s.SessionID }
}

var statelessSessionIDRE = regexp.MustCompile(
	`^mcp_([A-Za-z0-9.-]+)_v_([A-Za-z0-9.-]*)_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`,
)
var statelessSessionSeedRE = regexp.MustCompile(
	`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
)

const anonymousStatelessClient = "unknown"

// BuildStatelessSessionID mints an identity-bearing MCP session ID. Client
// attribution is observability only; callers must not treat the ID as auth.
func BuildStatelessSessionID(clientInfo *ClientInfo) string {
	return buildStatelessSessionID(clientInfo, "")
}

func buildStatelessSessionID(clientInfo *ClientInfo, sessionSeed string) string {
	name, version := anonymousStatelessClient, ""
	if clientInfo != nil {
		name = slugSessionPart(clientInfo.Name, anonymousStatelessClient)
		version = slugSessionPart(clientInfo.Version, "")
	}
	sessionUUID := strings.ToLower(strings.TrimSpace(sessionSeed))
	if !statelessSessionSeedRE.MatchString(sessionUUID) {
		sessionUUID = randomUUID()
	}
	return "mcp_" + name + "_v_" + version + "_" + sessionUUID
}

// ParseStatelessSessionClientInfo recovers best-effort client identity from an
// Armature stateless session ID. Anonymous or malformed IDs return nil.
func ParseStatelessSessionClientInfo(sessionID string) *ClientInfo {
	match := statelessSessionIDRE.FindStringSubmatch(sessionID)
	if match == nil || match[1] == anonymousStatelessClient {
		return nil
	}
	return &ClientInfo{Name: match[1], Version: match[2]}
}

// ResolveStatelessHTTPSession mints a session on initialize and otherwise
// recovers the ID and client identity from the echoed Mcp-Session-Id header.
// A missing echo receives a one-off UUID so unrelated requests never merge;
// when Headers is non-nil, that fallback is injected into the current request
// for framework adapters and transports to observe.
func ResolveStatelessHTTPSession(input StatelessHTTPInput) StatelessHTTPSession {
	body := decodeStatelessBody(input.Body)
	if initialize := findInitializeMessage(body); initialize != nil {
		clientInfo := clientInfoFromInitialize(initialize)
		sessionID := buildStatelessSessionID(clientInfo, input.Headers.Get("X-Armature-Session-Seed"))
		return StatelessHTTPSession{SessionID: sessionID, IsInitialize: true}
	}
	sessionID := strings.TrimSpace(input.Headers.Get("Mcp-Session-Id"))
	if sessionID == "" {
		sessionID = randomUUID()
		if input.Headers != nil {
			input.Headers.Set("Mcp-Session-Id", sessionID)
		}
	}
	return StatelessHTTPSession{
		SessionID:  sessionID,
		ClientInfo: ParseStatelessSessionClientInfo(sessionID),
	}
}

func decodeStatelessBody(body any) any {
	switch value := body.(type) {
	case json.RawMessage:
		var decoded any
		if json.Unmarshal(value, &decoded) == nil {
			return decoded
		}
	case []byte:
		var decoded any
		if json.Unmarshal(value, &decoded) == nil {
			return decoded
		}
	}
	return body
}

func findInitializeMessage(body any) map[string]any {
	messages, ok := body.([]any)
	if !ok {
		messages = []any{body}
	}
	for _, message := range messages {
		item, ok := message.(map[string]any)
		if ok && item["method"] == "initialize" {
			return item
		}
	}
	return nil
}

func clientInfoFromInitialize(message map[string]any) *ClientInfo {
	params, _ := message["params"].(map[string]any)
	info, _ := params["clientInfo"].(map[string]any)
	if info == nil {
		return nil
	}
	name, _ := info["name"].(string)
	version, _ := info["version"].(string)
	return &ClientInfo{Name: name, Version: version}
}

func slugSessionPart(value, fallback string) string {
	value = strings.TrimSpace(value)
	var out strings.Builder
	lastDash := false
	for _, r := range value {
		allowed := r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-'
		if allowed {
			out.WriteRune(r)
			lastDash = r == '-'
		} else if out.Len() > 0 && !lastDash {
			out.WriteByte('-')
			lastDash = true
		}
		if out.Len() >= 48 {
			break
		}
	}
	result := strings.Trim(out.String(), "-")
	if result == "" {
		return fallback
	}
	return result
}

func randomUUID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("armatureanalytics: crypto/rand failed: " + err.Error())
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	var encoded [36]byte
	hex.Encode(encoded[0:8], raw[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], raw[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], raw[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], raw[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], raw[10:16])
	return string(encoded[:])
}
