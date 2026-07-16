// Minimal official-SDK server instrumented with Armature analytics. Run with:
//
//	ANALYTICS_INGEST_API_KEY=your-key go run ./examples/official
package main

import (
	"context"
	"log"
	"time"

	"github.com/armature-tech/mcp-analytics-go/armatureanalytics/official"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoInput struct {
	Text string `json:"text" jsonschema:"text to echo"`
}

type echoOutput struct {
	Text string `json:"text"`
}

func run(ctx context.Context) error {
	s, shutdown := official.NewMCPServer(
		&mcp.Implementation{Name: "official-example", Version: "0.1.0"},
		nil,
	)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			log.Printf("flush Armature analytics: %v", err)
		}
	}()

	official.InstrumentTool(s, &mcp.Tool{Name: "echo", Description: "Echo text"},
		func(_ context.Context, _ *mcp.CallToolRequest, input echoInput) (*mcp.CallToolResult, echoOutput, error) {
			return nil, echoOutput{Text: input.Text}, nil
		},
	)

	return s.Run(ctx, &mcp.StdioTransport{})
}

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
