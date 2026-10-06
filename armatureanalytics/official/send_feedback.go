package official

import (
	"context"
	"strings"

	armatureanalytics "github.com/armature-tech/mcp-analytics-go/armatureanalytics"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type sendFeedbackInput struct {
	Capability string `json:"capability"`
}

// sendFeedbackEnabled resolves Config.SendFeedback (falling back to the
// deprecated RequestCapability alias): on unless set to false.
func sendFeedbackEnabled(cfg Config) bool {
	setting := cfg.SendFeedback
	if setting == nil {
		setting = cfg.RequestCapability
	}
	return setting == nil || *setting
}

func addSendFeedbackTool(s *mcp.Server, recorder *Recorder) {
	falseValue := false
	if s == nil {
		return
	}
	// The official SDK exposes last-write-wins registration and no tool lookup.
	// Injection happens on a newly constructed server, so a later customer
	// registration intentionally replaces this tool. Result-scoped provenance
	// in Recorder ensures that replacement is never reported as SDK demand.
	mcp.AddTool(s, &mcp.Tool{
		Name:        armatureanalytics.SendFeedbackToolName,
		Description: armatureanalytics.SendFeedbackToolDescription,
		// Directories such as ChatGPT's reject tools without explicit
		// readOnlyHint, destructiveHint and openWorldHint. It records an
		// analytics event (not read-only), changes no user data and reaches
		// no one outside the server.
		Annotations: &mcp.ToolAnnotations{
			Title:           armatureanalytics.SendFeedbackToolTitle,
			ReadOnlyHint:    false,
			DestructiveHint: &falseValue,
			IdempotentHint:  false,
			OpenWorldHint:   &falseValue,
		},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				armatureanalytics.SendFeedbackArgumentName: map[string]any{
					"type":        "string",
					"description": armatureanalytics.SendFeedbackArgumentDescription,
					"minLength":   1,
					"maxLength":   armatureanalytics.SendFeedbackArgumentMaxLength,
				},
			},
			"required":             []string{armatureanalytics.SendFeedbackArgumentName},
			"additionalProperties": false,
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input sendFeedbackInput) (*mcp.CallToolResult, any, error) {
		var reservation *armatureanalytics.CapabilityReservation
		if recorder != nil && recorder.core != nil {
			reservation = recorder.core.ReserveCapabilityRequest()
		}
		state, _ := ctx.Value(capabilityCallStateKey{}).(*capabilityCallState)
		if reservation == nil || !state.attach(reservation) {
			if reservation != nil {
				reservation.Release()
			}
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{
					Text: "capability request unavailable: analytics recorder is not active",
				}},
			}, nil, nil
		}
		if strings.TrimSpace(input.Capability) == "" || len(input.Capability) > armatureanalytics.SendFeedbackArgumentMaxLength {
			result := &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{
					Text: "capability must be a non-empty string of at most 1000 characters",
				}},
			}
			return result, nil, nil
		}
		result := &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Capability request acknowledged."}},
		}
		return result, nil, nil
	})
	armatureanalytics.MarkSendFeedbackRegistered(s)
}
