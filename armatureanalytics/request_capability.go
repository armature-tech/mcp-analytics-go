package armatureanalytics

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"weak"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const (
	requestCapabilityToolName        = "request_capability"
	requestCapabilityToolDescription = "Records that the user asked for something these tools cannot do, so the developers of this server can add it. It changes no data and contacts no one. Call it whenever you cannot do what the user asked with these tools, including when you send them to an app, a website or a manual step instead. Then answer them as usual."
	requestCapabilityArgDescription  = "One English sentence describing the missing capability needed for the user's task. Translate the summary into English even when the user writes in another language. Describe generic actions and roles. Omit names, contacts, IDs, credentials and all tool argument values."
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
// Keys are weak pointers and a GC cleanup deletes them, so a server nobody
// shuts down through this SDK is not retained.
var requestCapabilityServers sync.Map // weak.Pointer[T] → struct{}

// MarkRequestCapabilityRegistered records that the SDK-owned request_capability
// tool is registered on s. AddRequestCapabilityTool and the official adapter
// call it; InstrumentTool then points each tool's hint at request_capability.
func MarkRequestCapabilityRegistered[T any](s *T) {
	if s == nil {
		return
	}
	key := weak.Make(s)
	if _, loaded := requestCapabilityServers.LoadOrStore(key, struct{}{}); !loaded {
		runtime.AddCleanup(s, func(key weak.Pointer[T]) { requestCapabilityServers.Delete(key) }, key)
	}
}

// ForgetRequestCapabilityServer drops s from that registry (server shutdown).
func ForgetRequestCapabilityServer[T any](s *T) {
	if s != nil {
		requestCapabilityServers.Delete(weak.Make(s))
	}
}

// RequestCapabilityRegistered reports whether MarkRequestCapabilityRegistered
// ran for s.
func RequestCapabilityRegistered[T any](s *T) bool {
	if s == nil {
		return false
	}
	_, ok := requestCapabilityServers.Load(weak.Make(s))
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
