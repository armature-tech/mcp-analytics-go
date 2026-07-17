package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/armature-tech/mcp-analytics-go/armatureanalytics"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func toolResult(value any) *mcp.CallToolResult {
	raw, _ := json.Marshal(value)
	return mcp.NewToolResultText(string(raw))
}

func Handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Keep this test-org-only canary visible in Sessions. Production servers
	// retain the workflow header so synthetic traffic stays out of customer
	// session analytics.
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
	deployment := os.Getenv("SDK_CANARY_DEPLOYMENT")
	if deployment == "" {
		deployment = "unknown"
	}
	intent := deployment
	if !strings.HasPrefix(intent, "sdk-canary/") {
		intent = "sdk-canary/go/" + deployment
	}
	endpoint := os.Getenv("SDK_CANARY_PLATFORM_URL")
	if endpoint == "" {
		endpoint = "https://app.armature.tech"
	}
	endpoint = strings.TrimRight(endpoint, "/") + "/api/mcp-analytics/ingest"
	mcpServer, shutdown := armatureanalytics.NewMCPServerWithConfig(
		"sdk-canary-go",
		deployment,
		armatureanalytics.Config{
			APIKey:      os.Getenv("SDK_CANARY_INGEST_KEY"),
			EndpointURL: endpoint,
			Delivery:    armatureanalytics.DeliveryAwait,
			ActorSeed:   func(context.Context) string { return "sdk-canary-browser-worker" },
			Timeout:     10 * time.Second,
		},
		server.WithToolCapabilities(true),
	)
	armatureanalytics.InstrumentTool(
		mcpServer,
		mcp.NewTool("canary_identity", mcp.WithDescription("Call exactly once to get this MCP session identity. Reuse the result; do not retry or call this tool again. Set telemetry.user_intent exactly to "+intent+".")),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return toolResult(map[string]string{
				"package":    "go",
				"deployment": deployment,
				"session_id": session.SessionID,
				"next_step":  "Call canary_echo exactly once. Do not call canary_identity again.",
			}), nil
		},
	)
	armatureanalytics.InstrumentTool(
		mcpServer,
		mcp.NewTool("canary_echo", mcp.WithDescription("Call exactly once after canary_identity to echo a marker. Set telemetry.user_intent exactly to "+intent+"."), mcp.WithString("marker", mcp.Required())),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return toolResult(map[string]string{"marker": req.GetString("marker", ""), "session_id": session.SessionID, "deployment": deployment}), nil
		},
	)
	handler := server.NewStreamableHTTPServer(
		mcpServer,
		server.WithSessionIdManager(session.Mark3labsSessionIDManager()),
		server.WithDisableStreaming(true),
	)
	handler.ServeHTTP(w, r)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = shutdown(ctx)
}
