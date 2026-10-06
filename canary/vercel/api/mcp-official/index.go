// Package handler serves the official-SDK canary endpoint: a go-sdk v1.7.0
// StreamableHTTPHandler{Stateless: true} server. That transport mints no
// Mcp-Session-Id for legacy-era clients (GetSessionID is never consulted), so
// the analytics repair middleware WrapStatelessHTTPHandler is REQUIRED here
// for legacy-client session attribution — it is the very wiring this canary
// exists to exercise. The mark3labs canary at /mcp is unchanged.
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/armature-tech/mcp-analytics-go/armatureanalytics"
	"github.com/armature-tech/mcp-analytics-go/armatureanalytics/official"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type identityInput struct{}

type echoInput struct {
	Marker string `json:"marker" jsonschema:"marker to echo back"`
}

func textResult(value any) *mcp.CallToolResult {
	raw, _ := json.Marshal(value)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}
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
	cfg := armatureanalytics.Config{
		APIKey:      os.Getenv("SDK_CANARY_INGEST_KEY"),
		EndpointURL: endpoint,
		Delivery:    armatureanalytics.DeliveryAwait,
		ActorSeed:   func(context.Context) string { return "sdk-canary-browser-worker" },
		Timeout:     10 * time.Second,
		// The HTTP smoke test asserts an exact tool list; keep the
		// on-by-default send_feedback tool out of this fixture.
		SendFeedback: new(bool),
	}
	// A missing key disables recording but keeps the middleware's minting so
	// the endpoint still behaves like a session-issuing MCP server.
	rec, err := armatureanalytics.NewRecorder(cfg)
	if err != nil {
		rec = nil
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reuse the middleware's resolution: resolving the initialize body
		// again here would mint a DIFFERENT id than the response header.
		session, _ := armatureanalytics.StatelessHTTPSessionFromRequest(r)
		mcpServer, shutdown := official.NewMCPServerWithConfig(
			&mcp.Implementation{Name: "sdk-canary-go-official", Version: deployment},
			nil,
			cfg,
		)
		official.InstrumentTool(mcpServer,
			&mcp.Tool{Name: "canary_identity", Description: "Call exactly once to get this MCP session identity. Reuse the result; do not retry or call this tool again. Set telemetry.user_intent exactly to " + intent + "."},
			func(context.Context, *mcp.CallToolRequest, identityInput) (*mcp.CallToolResult, any, error) {
				return textResult(map[string]string{
					"package":    "go",
					"deployment": deployment,
					"session_id": session.SessionID,
					"next_step":  "Call canary_echo exactly once. Do not call canary_identity again.",
				}), nil, nil
			},
		)
		official.InstrumentTool(mcpServer,
			&mcp.Tool{Name: "canary_echo", Description: "Call exactly once after canary_identity to echo a marker. Omit telemetry.user_intent because this continues the same user turn."},
			func(_ context.Context, _ *mcp.CallToolRequest, input echoInput) (*mcp.CallToolResult, any, error) {
				return textResult(map[string]string{"marker": input.Marker, "session_id": session.SessionID, "deployment": deployment}), nil, nil
			},
		)
		handler := mcp.NewStreamableHTTPHandler(
			func(*http.Request) *mcp.Server { return mcpServer },
			&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
		)
		handler.ServeHTTP(w, r)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	})

	rec.WrapStatelessHTTPHandler(inner).ServeHTTP(w, r)
	if rec != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = rec.Close(ctx)
	}
}
