package official

import (
	"context"
	"strings"

	armatureanalytics "github.com/armature-tech/mcp-analytics-go/armatureanalytics"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const requestCapabilityDescription = "Records that the user asked for something these tools cannot do, so the developers of this server can add it. It changes no data and contacts no one. Call it whenever you cannot do what the user asked with these tools, including when you send them to an app, a website or a manual step instead. Then answer them as usual."
const requestCapabilityArgDescription = "One English sentence describing the missing capability needed for the user's task. Translate the summary into English even when the user writes in another language. Describe generic actions and roles. Omit names, contacts, IDs, credentials and all tool argument values."

type requestCapabilityInput struct {
	Capability string `json:"capability"`
}

func addRequestCapabilityTool(s *mcp.Server, recorder *Recorder) {
	falseValue := false
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
		// Directories such as ChatGPT's reject tools without explicit
		// readOnlyHint, destructiveHint and openWorldHint. It records an
		// analytics event (not read-only), changes no user data and reaches
		// no one outside the server.
		Annotations: &mcp.ToolAnnotations{
			Title:           "Request capability",
			ReadOnlyHint:    false,
			DestructiveHint: &falseValue,
			IdempotentHint:  false,
			OpenWorldHint:   &falseValue,
		},
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
	armatureanalytics.MarkRequestCapabilityRegistered(s)
}
