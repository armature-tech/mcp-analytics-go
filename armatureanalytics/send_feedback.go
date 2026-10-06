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

// The SDK-owned feedback tool. These strings are the cross-SDK contract
// (TELEMETRY-CONTRACT.md, "send_feedback"): every SDK and the hosted MCP
// carry byte-identical copies.
const (
	// SendFeedbackToolName is the name of the SDK-owned feedback tool.
	// Earlier releases named it request_capability.
	SendFeedbackToolName = "send_feedback"
	// SendFeedbackToolTitle is the tool's title annotation.
	SendFeedbackToolTitle = "Send feedback"
	// SendFeedbackToolDescription is the tool's description.
	SendFeedbackToolDescription = "Records that the user asked for something these tools cannot do, so the developers of this server can add it. It changes no data and contacts no one. Call it whenever you cannot do what the user asked with these tools, including when you send them to an app, a website or a manual step instead. Then answer them as usual."
	// SendFeedbackArgumentName is the tool's one required string argument.
	SendFeedbackArgumentName = "capability"
	// SendFeedbackArgumentDescription is that argument's description.
	SendFeedbackArgumentDescription = "One English sentence describing the missing capability needed for the user's task. Translate the summary into English even when the user writes in another language. Describe generic actions and roles. Omit names, contacts, IDs, credentials and all tool argument values."
	// SendFeedbackArgumentMaxLength caps the argument, in bytes.
	SendFeedbackArgumentMaxLength = 1000

	sendFeedbackResultMarker = "armature.dev/send-feedback"
)

// AddSendFeedbackTool adds the SDK-owned send_feedback tool to an existing
// mcp-go server. Pass the installed Recorder so its hooks can mark calls from
// this SDK-owned handler without confusing a later customer tool that reuses
// the same name. If the server already has a tool with this reserved name,
// registration fails. No other tool's description mentions send_feedback; a
// server listed in a connector directory that keeps it should mention it in
// its listing as a feedback tool.
func AddSendFeedbackTool(s *server.MCPServer, recorder *Recorder) error {
	if s == nil {
		return fmt.Errorf("send_feedback: server is nil")
	}
	if !recorder.canRecord() {
		return fmt.Errorf("send_feedback: recorder is not active")
	}
	if s.GetTool(SendFeedbackToolName) != nil {
		return fmt.Errorf("tool name %q is reserved while SendFeedback is enabled; rename your tool or set SendFeedback to false", SendFeedbackToolName)
	}

	s.AddTool(
		mcp.NewTool(
			SendFeedbackToolName,
			mcp.WithDescription(SendFeedbackToolDescription),
			// Directories such as ChatGPT's reject tools without explicit
			// readOnlyHint, destructiveHint and openWorldHint, and mcp-go's
			// defaults mark a tool destructive and open-world. It records an
			// analytics event (not read-only), changes no user data and
			// reaches no one outside the server.
			mcp.WithTitleAnnotation(SendFeedbackToolTitle),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(false),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithString(
				SendFeedbackArgumentName,
				mcp.Required(),
				mcp.Description(SendFeedbackArgumentDescription),
				mcp.MinLength(1),
				mcp.MaxLength(SendFeedbackArgumentMaxLength),
			),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			reservation := recorder.ReserveCapabilityRequest()
			if reservation == nil {
				return mcp.NewToolResultError("capability request unavailable: analytics recorder is not active"), nil
			}
			capability, ok := req.GetArguments()[SendFeedbackArgumentName].(string)
			if !ok || strings.TrimSpace(capability) == "" || len(capability) > SendFeedbackArgumentMaxLength {
				result := mcp.NewToolResultError("capability must be a non-empty string of at most 1000 characters")
				markCapabilityResult(reservation, result)
				return result, nil
			}
			result := mcp.NewToolResultText("Capability request acknowledged.")
			markCapabilityResult(reservation, result)
			return result, nil
		},
	)
	MarkSendFeedbackRegistered(s)
	return nil
}

// AddRequestCapabilityTool is AddSendFeedbackTool.
//
// Deprecated: the tool is now named send_feedback. Use AddSendFeedbackTool.
func AddRequestCapabilityTool(s *server.MCPServer, recorder *Recorder) error {
	return AddSendFeedbackTool(s, recorder)
}

// injectSendFeedback adds send_feedback to a server built by
// NewMCPServerWithConfig. On by default, it yields silently to a customer tool
// of the same name or an inactive recorder; explicitly enabled, those are
// reported through OnError.
func injectSendFeedback(s *server.MCPServer, rec *Recorder, cfg Config) {
	if !cfg.sendFeedbackEnabled() || cfg.Disabled || rec == nil {
		return
	}
	if err := AddSendFeedbackTool(s, rec); err != nil && cfg.sendFeedbackExplicit() && cfg.OnError != nil {
		cfg.OnError(err, Batch{})
	}
}

// sendFeedbackServers records the servers the SDK-owned send_feedback tool is
// registered on (a config with a delivery path does not prove that). Keys are
// weak pointers and a GC cleanup deletes them, so a server nobody shuts down
// through this SDK is not retained.
var sendFeedbackServers sync.Map // weak.Pointer[T] → struct{}

// MarkSendFeedbackRegistered records that the SDK-owned send_feedback tool is
// registered on s. AddSendFeedbackTool and the official adapter call it. Tool
// descriptions do not depend on it.
func MarkSendFeedbackRegistered[T any](s *T) {
	if s == nil {
		return
	}
	key := weak.Make(s)
	if _, loaded := sendFeedbackServers.LoadOrStore(key, struct{}{}); !loaded {
		runtime.AddCleanup(s, func(key weak.Pointer[T]) { sendFeedbackServers.Delete(key) }, key)
	}
}

// ForgetSendFeedbackServer drops s from that registry (server shutdown).
func ForgetSendFeedbackServer[T any](s *T) {
	if s != nil {
		sendFeedbackServers.Delete(weak.Make(s))
	}
}

// SendFeedbackRegistered reports whether MarkSendFeedbackRegistered ran for s.
func SendFeedbackRegistered[T any](s *T) bool {
	if s == nil {
		return false
	}
	_, ok := sendFeedbackServers.Load(weak.Make(s))
	return ok
}

// MarkRequestCapabilityRegistered is MarkSendFeedbackRegistered.
//
// Deprecated: use MarkSendFeedbackRegistered.
func MarkRequestCapabilityRegistered[T any](s *T) { MarkSendFeedbackRegistered(s) }

// ForgetRequestCapabilityServer is ForgetSendFeedbackServer.
//
// Deprecated: use ForgetSendFeedbackServer.
func ForgetRequestCapabilityServer[T any](s *T) { ForgetSendFeedbackServer(s) }

// RequestCapabilityRegistered is SendFeedbackRegistered.
//
// Deprecated: use SendFeedbackRegistered.
func RequestCapabilityRegistered[T any](s *T) bool { return SendFeedbackRegistered(s) }

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
	result.Meta.AdditionalFields[sendFeedbackResultMarker] = reservation
}
