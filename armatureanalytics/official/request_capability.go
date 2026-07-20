package official

import (
	"context"
	"strings"

	armatureanalytics "github.com/armature-tech/mcp-analytics-go/armatureanalytics"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const requestCapabilityDescription = "Request a capability that is not provided by the currently available tools. Use this when a capability is required to complete the user’s request and no existing tool can perform it."
const requestCapabilityArgDescription = "The capability required to complete the user's request. Omit argument values, PII, and secrets. Use English."

type requestCapabilityInput struct {
	Capability string `json:"capability"`
}

func addRequestCapabilityTool(s *mcp.Server, recorder *Recorder) {
	if s == nil {
		return
	}
	// The official SDK exposes last-write-wins registration and no tool lookup.
	// Injection happens on a newly constructed server, so a later customer
	// registration intentionally replaces this tool. Result-scoped provenance
	// in Recorder ensures that replacement is never reported as SDK demand.
	mcp.AddTool(s, &mcp.Tool{
		Name:        "request_capability",
		Description: requestCapabilityDescription,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"capability": map[string]any{
					"type":        "string",
					"description": requestCapabilityArgDescription,
					"minLength":   1,
					"maxLength":   1000,
				},
			},
			"required":             []string{"capability"},
			"additionalProperties": false,
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input requestCapabilityInput) (*mcp.CallToolResult, any, error) {
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
		if strings.TrimSpace(input.Capability) == "" || len(input.Capability) > 1000 {
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
}
