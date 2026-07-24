package integration_test

import (
	"context"
	"sync"
	"testing"
	"time"

	armatureanalytics "github.com/armature-tech/mcp-analytics-go/armatureanalytics"
	"github.com/armature-tech/mcp-analytics-go/armatureanalytics/official"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestOfficialAdapterExternalConsumer(t *testing.T) {
	var mu sync.Mutex
	var events []armatureanalytics.Event
	server, shutdown := official.NewMCPServerWithConfig(
		&mcp.Implementation{Name: "official-canary", Version: "1"},
		nil,
		official.Config{
			Delivery:          armatureanalytics.DeliveryAwait,
			RequestCapability: boolPtr(false),
			ActorSeed:         func(context.Context) string { return "sdk-canary-shared-actor" },
			Emit: func(_ context.Context, batch armatureanalytics.Batch) error {
				mu.Lock()
				defer mu.Unlock()
				events = append(events, batch.Events...)
				return nil
			},
		},
	)
	official.InstrumentTool(server, &mcp.Tool{Name: "canary_echo"},
		func(_ context.Context, _ *mcp.CallToolRequest, input map[string]any) (*mcp.CallToolResult, map[string]any, error) {
			if _, leaked := input["telemetry"]; leaked {
				t.Fatal("telemetry leaked into official SDK handler")
			}
			return nil, map[string]any{"marker": input["marker"]}, nil
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "sdk-publish-canary", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 1 {
		t.Fatalf("tools/list returned %d tools", len(listed.Tools))
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "canary_echo", Arguments: map[string]any{
		"marker":    "official/call-1",
		"telemetry": map[string]any{"user_intent": "sdk-canary/go/official/local"},
	}})
	if err != nil || result.IsError {
		t.Fatalf("tools/call failed: result=%#v err=%v", result, err)
	}
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var sawInit, sawCall bool
	for _, event := range events {
		if event.Kind == armatureanalytics.KindSessionInit {
			sawInit = true
		}
		if event.Kind == armatureanalytics.KindToolCall && event.Metadata["user_intent"] == "sdk-canary/go/official/local" {
			sawCall = true
		}
	}
	if !sawInit || !sawCall {
		t.Fatalf("missing official adapter events: %#v", events)
	}
}
