package armatureanalytics

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const (
	requestCapabilityToolName        = "request_capability"
	requestCapabilityToolDescription = "Request a capability that is not provided by the currently available tools. Use this when a capability is required to complete the user’s request and no existing tool can perform it."
	requestCapabilityArgDescription  = "The capability required to complete the user's request. Omit argument values, PII, and secrets. Use English."
	requestCapabilityResultMarker    = "armature.dev/request-capability"
)

// AddRequestCapabilityTool adds the opt-in request_capability tool to an
// existing mcp-go server. Pass the installed Recorder so its hooks can mark
// calls from this SDK-owned handler without confusing a later customer tool
// that reuses the same name. If the server already has a tool with this
// reserved name, registration fails.
func AddRequestCapabilityTool(s *server.MCPServer, recorder *Recorder) error {
	if s == nil {
		return fmt.Errorf("request_capability: server is nil")
	}
	if !recorder.canRecord() {
		return fmt.Errorf("request_capability: recorder is not active")
	}
	if s.GetTool(requestCapabilityToolName) != nil {
		return fmt.Errorf("tool name %q is reserved while RequestCapability is enabled", requestCapabilityToolName)
	}

	s.AddTool(
		mcp.NewTool(
			requestCapabilityToolName,
			mcp.WithDescription(requestCapabilityToolDescription),
			mcp.WithString(
				"capability",
				mcp.Required(),
				mcp.Description(requestCapabilityArgDescription),
				mcp.MinLength(1),
				mcp.MaxLength(1000),
			),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			reservation := recorder.ReserveCapabilityRequest()
			if reservation == nil {
				return mcp.NewToolResultError("capability request unavailable: analytics recorder is not active"), nil
			}
			capability, ok := req.GetArguments()["capability"].(string)
			if !ok || strings.TrimSpace(capability) == "" || len(capability) > 1000 {
				result := mcp.NewToolResultError("capability must be a non-empty string of at most 1000 characters")
				markCapabilityResult(reservation, result)
				return result, nil
			}
			result := mcp.NewToolResultText("Capability request acknowledged.")
			markCapabilityResult(reservation, result)
			return result, nil
		},
	)
	return nil
}

func markCapabilityResult(reservation *CapabilityReservation, result *mcp.CallToolResult) {
	if reservation == nil || result == nil {
		return
	}
	if result.Meta == nil {
		result.Meta = &mcp.Meta{}
	}
	if result.Meta.AdditionalFields == nil {
		result.Meta.AdditionalFields = make(map[string]any)
	}
	result.Meta.AdditionalFields[requestCapabilityResultMarker] = reservation
}
