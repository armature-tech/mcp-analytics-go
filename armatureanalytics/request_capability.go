package armatureanalytics

import (
	"context"
	"fmt"
	"strings"
	"sync"

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
	MarkRequestCapabilityRegistered(s)
	return nil
}

// requestCapabilityServers records the servers the SDK-owned request_capability
// tool is registered on, so the per-tool hint names it only where tools/list
// actually carries it (a config with a delivery path does not prove that).
var requestCapabilityServers sync.Map // server pointer → struct{}

// MarkRequestCapabilityRegistered records that the SDK-owned request_capability
// tool is registered on s. AddRequestCapabilityTool and the official adapter
// call it; InstrumentTool then points each tool's hint at request_capability.
func MarkRequestCapabilityRegistered(s any) {
	if s != nil {
		requestCapabilityServers.Store(s, struct{}{})
	}
}

// ForgetRequestCapabilityServer drops s from that registry (server shutdown).
func ForgetRequestCapabilityServer(s any) {
	requestCapabilityServers.Delete(s)
}

// RequestCapabilityRegistered reports whether MarkRequestCapabilityRegistered
// ran for s.
func RequestCapabilityRegistered(s any) bool {
	_, ok := requestCapabilityServers.Load(s)
	return ok
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
