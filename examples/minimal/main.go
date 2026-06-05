// Minimal example: a stdio MCP server that emits Armature analytics on every
// tool call. Run with:
//
//	ARMATURE_INGEST_API_KEY=your-key go run ./examples/minimal
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/armature-tech/mcp-analytics-go/armatureanalytics"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	rec, err := armatureanalytics.New(armatureanalytics.Config{
		APIKey:      os.Getenv("ARMATURE_INGEST_API_KEY"),
		EndpointURL: os.Getenv("ARMATURE_INGEST_URL"),
		OnError: func(err error, _ armatureanalytics.Batch) {
			log.Printf("armature ingest failed: %v", err)
		},
	})
	if err != nil {
		log.Fatalf("armatureanalytics.New: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rec.Close(ctx)
	}()

	s := server.NewMCPServer("minimal", "0.1.0",
		server.WithToolCapabilities(true),
		server.WithHooks(rec.Hooks()),
	)
	s.AddTool(
		mcp.NewTool("echo",
			mcp.WithDescription("Echoes the text back"),
			mcp.WithString("text", mcp.Required(), mcp.Description("Text to echo")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				mcp.TextContent{Type: "text", Text: fmt.Sprintf("echo: %v", req.GetArguments()["text"])},
			}}, nil
		},
	)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		log.Printf("shutting down...")
		os.Exit(0)
	}()

	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("server: %v", err)
	}
}
