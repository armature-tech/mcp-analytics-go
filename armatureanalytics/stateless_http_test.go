package armatureanalytics

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
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

func TestResolveStatelessHTTPSessionSeedSurvivesProxyReconnect(t *testing.T) {
	seed := "11111111-2222-4333-8444-555555555555"
	body := map[string]any{
		"method": "initialize",
		"params": map[string]any{"clientInfo": map[string]any{
			"name": "mcp-tester-claude-remote-proxy", "version": "0.1.0",
		}},
	}
	resolve := func() StatelessHTTPSession {
		return ResolveStatelessHTTPSession(StatelessHTTPInput{
			Body:    body,
			Headers: http.Header{"X-Armature-Session-Seed": []string{seed}},
		})
	}
	first, reconnected := resolve(), resolve()
	if first.SessionID != reconnected.SessionID {
		t.Fatalf("reconnect split %q from %q", first.SessionID, reconnected.SessionID)
	}
	want := "mcp_mcp-tester-claude-remote-proxy_v_0.1.0_" + seed
	clientInfo := ParseStatelessSessionClientInfo(first.SessionID)
	if first.SessionID != want || clientInfo == nil || clientInfo.Name != "mcp-tester-claude-remote-proxy" {
		t.Fatalf("seeded session = %#v, want id %q with client info", first, want)
	}
}

func TestStatelessSessionSeedAcceptsTimeOrderedUUIDV7(t *testing.T) {
	seed := "019f942a-5d64-7322-8e50-5d17333768d9"
	got := buildStatelessSessionID(&ClientInfo{Name: "client"}, seed)
	if want := "mcp_client_v__" + seed; got != want {
		t.Fatalf("seeded session id = %q, want %q", got, want)
	}
}

func TestResolveStatelessHTTPSessionRejectsInvalidSeed(t *testing.T) {
	session := ResolveStatelessHTTPSession(StatelessHTTPInput{
		Body: map[string]any{
			"method": "initialize",
			"params": map[string]any{"clientInfo": map[string]any{"name": "client"}},
		},
		Headers: http.Header{"X-Armature-Session-Seed": []string{"attacker-controlled"}},
	})
	if !regexp.MustCompile(`^mcp_client_v__[0-9a-f-]{36}$`).MatchString(session.SessionID) || strings.Contains(session.SessionID, "attacker") {
		t.Fatalf("invalid seed controlled session id %q", session.SessionID)
	}
}

// A request with neither an initialize message nor an echoed Mcp-Session-Id
// is 2026-07-28 traffic (which removed both) or a client that lost the echo.
// The session resolves empty so ingest buckets it server-side — a minted
// per-request UUID would fabricate one session per POST.
func TestResolveStatelessHTTPSessionMissingEchoResolvesEmpty(t *testing.T) {
	headers := make(http.Header)
	session := ResolveStatelessHTTPSession(StatelessHTTPInput{
		Body:    map[string]any{"method": "tools/call"},
		Headers: headers,
	})
	if session.SessionID != "" || session.ClientInfo != nil || session.IsInitialize {
		t.Fatalf("fallback session = %#v, want empty", session)
	}
	if got := headers.Get("Mcp-Session-Id"); got != "" {
		t.Fatalf("no-echo request grew a fabricated session header %q", got)
	}
	if session.SessionIDGenerator() != nil {
		t.Fatal("sessionless request should not mint transport session ids")
	}
	manager := session.Mark3labsSessionIDManager()
	if terminated, err := manager.Validate(""); err != nil || terminated {
		t.Fatalf("sessionless mark3labs request rejected: terminated=%v err=%v", terminated, err)
	}
	if _, err := manager.Validate("mismatched"); err == nil {
		t.Fatal("non-empty header against an empty resolution should be rejected")
	}
}
