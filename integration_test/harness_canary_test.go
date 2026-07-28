package integration_test

// Permanent regression coverage for the CI job "Test Go on Vercel and in
// Armature": the real-harness canary drives the Vercel canary server
// (canary/vercel/api/mcp.go — the mark3labs stateless wiring) with two real
// agent clients on the legacy 2025 protocol, both sending
// X-Armature-Session-Seed and X-Armature-Workflow-Run-Id on every HTTP
// request (value = the workflow run id):
//
//   - the Claude Code leg goes through the Armature stdio→HTTP remote proxy,
//     whose TS-SDK StreamableHTTPClientTransport POSTs with
//     "Accept: application/json, text/event-stream" and opens a background
//     GET SSE stream after the initialized notification;
//   - the Codex leg is codex's own MCP client, which POSTs with a plain JSON
//     accept and no GET stream.
//
// The platform correlates each session to its workflow run purely through
// the seed embedded in the minted session key (the canary server strips the
// workflow header so the synthetic traffic stays visible in Sessions), so
// the load-bearing contract is: every request shape above must mint
// mcp_<client>_v_<version>_<run id> at initialize and keep every event of
// the conversation under that exact session hint.
//
// This test replicates the canary server verbatim (per-request server
// construction, workflow-header strip + seed mapping, custom
// SessionIdManager, streaming disabled) and drives both request shapes over
// raw HTTP so header handling matches the wire, not an SDK convenience
// layer.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/armature-tech/mcp-analytics-go/armatureanalytics"
	markmcp "github.com/mark3labs/mcp-go/mcp"
	markserver "github.com/mark3labs/mcp-go/server"
)

// harnessCanaryHandler mirrors canary/vercel/api/mcp.go Handler. Keep the
// two in lockstep: this is the code path the platform canary gate exercises
// with real agents.
func harnessCanaryHandler(t *testing.T, collector *statelessEventCollector) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		workflowRunID := r.Header.Get("X-Armature-Workflow-Run-Id")
		r.Header.Del("X-Armature-Workflow-Run-Id")
		if workflowRunID != "" {
			r.Header.Set("X-Armature-Session-Seed", workflowRunID)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var body any
		_ = json.Unmarshal(raw, &body)
		session := armatureanalytics.ResolveStatelessHTTPSession(armatureanalytics.StatelessHTTPInput{Body: body, Headers: r.Header})
		server, shutdown := armatureanalytics.NewMCPServerWithConfig(
			"sdk-canary-go",
			"harness-canary-test",
			armatureanalytics.Config{
				Delivery:          armatureanalytics.DeliveryAwait,
				Emit:              collector.emit,
				ActorSeed:         func(context.Context) string { return "sdk-canary-browser-worker" },
				RequestCapability: new(bool),
			},
			markserver.WithToolCapabilities(true),
		)
		armatureanalytics.InstrumentTool(
			server,
			markmcp.NewTool("canary_identity", markmcp.WithDescription("Call exactly once to get this MCP session identity.")),
			func(context.Context, markmcp.CallToolRequest) (*markmcp.CallToolResult, error) {
				return markmcp.NewToolResultText(session.SessionID), nil
			},
		)
		armatureanalytics.InstrumentTool(
			server,
			markmcp.NewTool("canary_echo", markmcp.WithDescription("Echo a marker."), markmcp.WithString("marker", markmcp.Required())),
			func(_ context.Context, req markmcp.CallToolRequest) (*markmcp.CallToolResult, error) {
				return markmcp.NewToolResultText(req.GetString("marker", "")), nil
			},
		)
		handler := markserver.NewStreamableHTTPServer(
			server,
			markserver.WithSessionIdManager(session.Mark3labsSessionIDManager()),
			markserver.WithDisableStreaming(true),
		)
		handler.ServeHTTP(w, r)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	})
}

// harnessLegClient drives one harness leg over raw HTTP, echoing any
// Mcp-Session-Id the server returns — the legacy-protocol client contract.
type harnessLegClient struct {
	t         *testing.T
	endpoint  string
	accept    string
	runID     string
	sessionID string
	nextID    int
}

func (c *harnessLegClient) post(body string, notification bool) (*http.Response, []byte) {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.endpoint, bytes.NewBufferString(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", c.accept)
	// The harness sends both custom headers on every request; the values are
	// the workflow run id (workers/src/cli/mcp-config.ts runScopedMcpHeaders).
	req.Header.Set("X-Armature-Session-Seed", c.runID)
	req.Header.Set("X-Armature-Workflow-Run-Id", c.runID)
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	if echoed := response.Header.Get("Mcp-Session-Id"); echoed != "" {
		c.sessionID = echoed
	}
	want := http.StatusOK
	if notification {
		want = http.StatusAccepted
	}
	if response.StatusCode != want {
		c.t.Fatalf("POST %s status = %d, body %s", body, response.StatusCode, payload)
	}
	return response, payload
}

func (c *harnessLegClient) rpc(method, params string) []byte {
	c.t.Helper()
	c.nextID++
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, c.nextID, method, params)
	_, payload := c.post(body, false)
	return payload
}

// openSSEStream mimics the TS SDK's background GET after the initialized
// notification. The canary server serves POST only; 405 is the tolerated
// answer (the TS SDK treats it as "no server-initiated stream").
func (c *harnessLegClient) openSSEStream() {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.endpoint, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Armature-Session-Seed", c.runID)
	req.Header.Set("X-Armature-Workflow-Run-Id", c.runID)
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		c.t.Fatalf("GET stream status = %d, want 405", response.StatusCode)
	}
}

func runHarnessLeg(t *testing.T, endpoint, accept, clientName, clientVersion, runID string, withSSEStream bool) string {
	t.Helper()
	client := &harnessLegClient{t: t, endpoint: endpoint, accept: accept, runID: runID}
	client.rpc("initialize", fmt.Sprintf(
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":%q,"version":%q}}`,
		clientName, clientVersion,
	))
	if client.sessionID == "" {
		t.Fatal("initialize did not issue Mcp-Session-Id")
	}
	wantSession := "mcp_" + clientName + "_v_" + clientVersion + "_" + runID
	if client.sessionID != wantSession {
		t.Fatalf("minted session id = %q, want the seed-embedding %q", client.sessionID, wantSession)
	}
	client.post(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`, true)
	if withSSEStream {
		client.openSSEStream()
	}
	client.rpc("tools/list", `{}`)
	client.rpc("tools/call", `{"name":"canary_identity","arguments":{"telemetry":{"user_intent":"sdk-canary/go/harness-test"}}}`)
	client.rpc("tools/call", fmt.Sprintf(`{"name":"canary_echo","arguments":{"marker":%q}}`, client.sessionID))
	return client.sessionID
}

func TestHarnessCanaryLegacyLegsHonorSessionSeed(t *testing.T) {
	collector := &statelessEventCollector{}
	httpServer := httptest.NewServer(harnessCanaryHandler(t, collector))
	defer httpServer.Close()

	const claudeRunID = "e210678c-b388-5cc3-833c-705437c9674d"
	const codexRunID = "8a8024ce-cb42-5980-8907-6a808b4d3012"

	// Claude Code leg: TS-SDK request shape (SSE-inclusive accept, background
	// GET stream after initialized).
	claudeSession := runHarnessLeg(t, httpServer.URL,
		"application/json, text/event-stream",
		"mcp-tester-claude-remote-proxy", "0.1.0", claudeRunID, true)

	// Codex leg: plain JSON accept, no GET stream.
	codexSession := runHarnessLeg(t, httpServer.URL,
		"application/json",
		"codex-mcp-client", "0.129.0", codexRunID, false)

	if claudeSession == codexSession {
		t.Fatalf("legs shared a session id: %q", claudeSession)
	}

	toolCalls := map[string][]string{}
	for _, event := range collector.snapshot() {
		if event.SessionIDHint == nil || *event.SessionIDHint == "" {
			t.Fatalf("event lost its seeded session hint: %#v", event)
		}
		hint := *event.SessionIDHint
		if hint != claudeSession && hint != codexSession {
			t.Fatalf("event minted an unseeded session %q (want %q or %q)", hint, claudeSession, codexSession)
		}
		var runID string
		if hint == claudeSession {
			runID = claudeRunID
		} else {
			runID = codexRunID
		}
		if !strings.HasSuffix(hint, "_"+runID) {
			t.Fatalf("session %q does not embed workflow run id %q", hint, runID)
		}
		// The canary strips X-Armature-Workflow-Run-Id before the adapter so
		// the traffic stays visible in Sessions; correlation is carried by the
		// seeded key alone.
		if _, present := event.Metadata["workflow_run_id"]; present {
			t.Fatalf("canary leaked workflow_run_id into event metadata: %#v", event.Metadata)
		}
		if event.Kind == armatureanalytics.KindToolCall {
			name, _ := event.Metadata["tool_name"].(string)
			toolCalls[hint] = append(toolCalls[hint], name)
		}
	}
	for session, wantRun := range map[string]string{claudeSession: claudeRunID, codexSession: codexRunID} {
		calls := toolCalls[session]
		if len(calls) != 2 || calls[0] != "canary_identity" || calls[1] != "canary_echo" {
			t.Fatalf("session %s (run %s) tool calls = %v, want [canary_identity canary_echo]", session, wantRun, calls)
		}
	}
}
