package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/armature-tech/mcp-analytics-go/armatureanalytics"
)

func TestProductionPlatformSessionIsolation(t *testing.T) {
	ingestKey := os.Getenv("SDK_CANARY_INGEST_KEY")
	readKey := os.Getenv("SDK_CANARY_READ_API_KEY")
	serverID := os.Getenv("SDK_CANARY_MCP_SERVER_ID")
	if ingestKey == "" || readKey == "" || serverID == "" {
		t.Skip("production canary credentials are not configured")
	}
	base := os.Getenv("SDK_CANARY_PLATFORM_URL")
	if base == "" {
		base = "https://app.armature.tech"
	}
	marker := fmt.Sprintf("sdk-canary/go/%s-%s-%d", os.Getenv("GITHUB_RUN_ID"), os.Getenv("GITHUB_RUN_ATTEMPT"), time.Now().UnixNano())
	recorder, err := armatureanalytics.NewRecorder(armatureanalytics.Config{
		APIKey: ingestKey, EndpointURL: base + "/api/mcp-analytics/ingest", Delivery: armatureanalytics.DeliveryAwait,
		ActorSeed: func(context.Context) string { return "sdk-canary-shared-actor" },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, session := range []string{"session-a", "session-b"} {
		for _, step := range []struct {
			call   string
			failed bool
		}{{"call-1", false}, {"call-2", true}} {
			call, failed := step.call, step.failed
			started := time.Now()
			telemetry := armatureanalytics.Telemetry{AgentThinking: "exercise the canary path"}
			if call == "call-1" {
				telemetry.UserIntent = marker
			}
			recorder.RecordToolCall(ctx, armatureanalytics.ToolCallInput{
				ToolName: map[bool]string{false: "canary_echo", true: "canary_expected_error"}[failed],
				Args:     map[string]any{"marker": session + "/" + call}, Result: map[string]any{"marker": session + "/" + call},
				IsToolError: failed, SessionID: marker + "/" + session, StartedAt: started, FinishedAt: time.Now(),
				Telemetry: telemetry,
			})
		}
	}
	if err := recorder.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	type sessionRow struct {
		ID, MCPServerID, SessionKey, ActorID, RawIntent string
		EventCount, OKCount, ErrorCount                 int
	}
	var matches []sessionRow
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		endpoint, _ := url.Parse(base + "/api/armature/v1/insights/sessions")
		query := endpoint.Query()
		query.Set("range", "24h")
		query.Set("intent", marker)
		query.Set("limit", "100")
		endpoint.RawQuery = query.Encode()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		req.Header.Set("Authorization", "Bearer "+readKey)
		response, requestErr := http.DefaultClient.Do(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		var body struct {
			Sessions []map[string]any `json:"sessions"`
		}
		decodeErr := json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if decodeErr != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("readback status=%d err=%v", response.StatusCode, decodeErr)
		}
		matches = nil
		for _, raw := range body.Sessions {
			if raw["raw_intent"] == marker && raw["mcp_server_id"] == serverID {
				matches = append(matches, sessionRow{ID: fmt.Sprint(raw["id"]), MCPServerID: serverID, SessionKey: fmt.Sprint(raw["session_key"]), ActorID: fmt.Sprint(raw["actor_id"]), RawIntent: marker, EventCount: int(raw["event_count"].(float64)), OKCount: int(raw["ok_count"].(float64)), ErrorCount: int(raw["error_count"].(float64))})
			}
		}
		if len(matches) == 2 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if len(matches) != 2 {
		if len(matches) == 0 {
			t.Fatal("expected two platform sessions, got 0 (ingest succeeded, so zero visible sessions usually means the canary organization is subject to a free-tier session-visibility cap; keep the canary org on a non-free plan)")
		}
		t.Fatalf("expected two platform sessions, got %d", len(matches))
	}
	if matches[0].SessionKey == matches[1].SessionKey || matches[0].ActorID != matches[1].ActorID {
		t.Fatal("platform mixed session or actor identity")
	}
	for _, session := range matches {
		if session.EventCount != 2 || session.OKCount != 1 || session.ErrorCount != 1 {
			t.Fatalf("unexpected counts: %#v", session)
		}
		traceRequest, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/armature/v1/insights/sessions/"+session.ID+"/trace", nil)
		traceRequest.Header.Set("Authorization", "Bearer "+readKey)
		traceResponse, err := http.DefaultClient.Do(traceRequest)
		if err != nil {
			t.Fatal(err)
		}
		var trace any
		decodeErr := json.NewDecoder(traceResponse.Body).Decode(&trace)
		traceResponse.Body.Close()
		if decodeErr != nil || traceResponse.StatusCode != http.StatusOK {
			t.Fatalf("trace status=%d err=%v", traceResponse.StatusCode, decodeErr)
		}
		traceJSON, _ := json.Marshal(trace)
		traceText := string(traceJSON)
		label := "session-b"
		if strings.Contains(traceText, "session-a") {
			label = "session-a"
		}
		other := map[string]string{"session-a": "session-b", "session-b": "session-a"}[label]
		if !strings.Contains(traceText, label+"/call-1") || !strings.Contains(traceText, label+"/call-2") || strings.Contains(traceText, other) {
			t.Fatalf("cross-session trace contamination in %s", session.ID)
		}
		t.Logf("platform session: %s/mcp-analytics/sessions/%s", base, session.ID)
	}
}
