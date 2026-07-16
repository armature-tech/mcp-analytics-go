package armatureanalytics

import (
	"encoding/json"
	"net/http"
	"regexp"
	"testing"
)

func TestStatelessSessionIDRoundTripsClientIdentity(t *testing.T) {
	sessionID := BuildStatelessSessionID(&ClientInfo{Name: "Claude Code", Version: "2.0.13"})
	if !regexp.MustCompile(`^mcp_Claude-Code_v_2\.0\.13_[0-9a-f-]{36}$`).MatchString(sessionID) {
		t.Fatalf("unexpected session id %q", sessionID)
	}
	info := ParseStatelessSessionClientInfo(sessionID)
	if info == nil || info.Name != "Claude-Code" || info.Version != "2.0.13" {
		t.Fatalf("parsed client info = %#v", info)
	}
}

func TestStatelessSessionAnonymousAndMalformedDoNotParse(t *testing.T) {
	if got := ParseStatelessSessionClientInfo(BuildStatelessSessionID(nil)); got != nil {
		t.Fatalf("anonymous id parsed as %#v", got)
	}
	for _, value := range []string{"", "session_123", "mcp_name_v_1_not-a-uuid"} {
		if got := ParseStatelessSessionClientInfo(value); got != nil {
			t.Errorf("ParseStatelessSessionClientInfo(%q) = %#v", value, got)
		}
	}
}

func TestResolveStatelessHTTPSessionInitializeAndBatch(t *testing.T) {
	for _, body := range []any{
		map[string]any{
			"method": "initialize",
			"params": map[string]any{"clientInfo": map[string]any{"name": "cursor", "version": "1.5"}},
		},
		json.RawMessage(`[{"method":"notifications/initialized"},{"method":"initialize","params":{"clientInfo":{"name":"cursor","version":"1.5"}}}]`),
	} {
		session := ResolveStatelessHTTPSession(StatelessHTTPInput{Body: body})
		if !session.IsInitialize || session.SessionIDGenerator() == nil {
			t.Fatalf("initialize session = %#v", session)
		}
		if got := session.SessionIDGenerator()(); got != session.SessionID {
			t.Fatalf("generator = %q, want %q", got, session.SessionID)
		}
		if !regexp.MustCompile(`^mcp_cursor_v_1\.5_`).MatchString(session.SessionID) {
			t.Fatalf("unexpected session id %q", session.SessionID)
		}
	}
}

func TestResolveStatelessHTTPSessionRecoversEchoedIdentity(t *testing.T) {
	issued := BuildStatelessSessionID(&ClientInfo{Name: "claude-code", Version: "2.0.13"})
	session := ResolveStatelessHTTPSession(StatelessHTTPInput{
		Body:    map[string]any{"method": "tools/call"},
		Headers: http.Header{"Mcp-Session-Id": []string{issued}},
	})
	if session.IsInitialize || session.SessionID != issued || session.SessionIDGenerator() != nil {
		t.Fatalf("tool session = %#v", session)
	}
	if session.ClientInfo == nil || session.ClientInfo.Name != "claude-code" {
		t.Fatalf("client info = %#v", session.ClientInfo)
	}
	manager := session.Mark3labsSessionIDManager()
	if got := manager.Generate(); got != issued {
		t.Fatalf("mark3labs generator = %q, want %q", got, issued)
	}
	if terminated, err := manager.Validate(issued); err != nil {
		t.Fatalf("mark3labs validation rejected echoed id: %v", err)
	} else if terminated {
		t.Fatal("mark3labs reported active echoed id as terminated")
	}
	if _, err := manager.Validate("wrong"); err == nil {
		t.Fatal("mark3labs validation accepted wrong id")
	}
}

func TestResolveStatelessHTTPSessionMissingEchoUsesOneOffUUID(t *testing.T) {
	headers := make(http.Header)
	session := ResolveStatelessHTTPSession(StatelessHTTPInput{
		Body:    map[string]any{"method": "tools/call"},
		Headers: headers,
	})
	if !regexp.MustCompile(`^[0-9a-f-]{36}$`).MatchString(session.SessionID) || session.ClientInfo != nil {
		t.Fatalf("fallback session = %#v", session)
	}
	if got := headers.Get("Mcp-Session-Id"); got != session.SessionID {
		t.Fatalf("request fallback header = %q, want %q", got, session.SessionID)
	}
}
