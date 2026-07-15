# Armature MCP Analytics for Go

Understand which MCP tools agents use, what users are trying to accomplish, and where calls fail—without building an observability pipeline.

[![Go Reference](https://pkg.go.dev/badge/github.com/armature-tech/mcp-analytics-go.svg)](https://pkg.go.dev/github.com/armature-tech/mcp-analytics-go/armatureanalytics)
[![CI](https://github.com/armature-tech/mcp-analytics-go/actions/workflows/ci.yml/badge.svg)](https://github.com/armature-tech/mcp-analytics-go/actions/workflows/ci.yml)
[![GitHub release](https://img.shields.io/github/v/release/armature-tech/mcp-analytics-go)](https://github.com/armature-tech/mcp-analytics-go/releases)
[![Apache 2.0](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

[Dashboard](https://app.armature.tech) · [TypeScript SDK](https://github.com/armature-tech/mcp-analytics) · [Python SDK](https://github.com/armature-tech/mcp-analytics-python) · [Agent install](SKILL.md)

Built for Go MCP servers using [mark3labs/mcp-go](https://github.com/mark3labs/mcp-go).

## Install in 30 seconds

### 1. Install

~~~bash
go get github.com/armature-tech/mcp-analytics-go
~~~

### 2. Add your ingest key

Create a server in the [Armature dashboard](https://app.armature.tech), copy its ingest key, and add it to your environment:

~~~bash
export ANALYTICS_INGEST_API_KEY="..."
~~~

### 3. Instrument your MCP server

~~~go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    "github.com/armature-tech/mcp-analytics-go/armatureanalytics"
    "github.com/mark3labs/mcp-go/mcp"
    "github.com/mark3labs/mcp-go/server"
)

func main() {
    s, shutdown := armatureanalytics.NewMCPServer(
        "Customer MCP",
        "1.0.0",
        server.WithToolCapabilities(true),
    )
    defer func() {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        _ = shutdown(ctx)
    }()

    armatureanalytics.InstrumentTool(
        s,
        mcp.NewTool(
            "lookup_customer",
            mcp.WithString("customer_id", mcp.Required()),
        ),
        func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
            id := req.GetArguments()["customer_id"]
            return mcp.NewToolResultText(fmt.Sprintf("customer %v is active", id)), nil
        },
    )

    if err := server.ServeStdio(s); err != nil {
        log.Fatal(err)
    }
}
~~~

> **That’s it. Make one tool call, open Armature, and the session is already there.**

## Built for MCP—not page views

| Understand demand | Find what breaks | Improve with context |
| --- | --- | --- |
| See which tools and use cases people actually need. | Surface failures, retries, latency, and dead ends. | Connect every call to user intent and agent reasoning. |

No custom event schema. No logging pipeline. No changes to your tool handlers.

## What you see in Armature

- Complete MCP sessions and client attribution
- The user intent behind each session
- Every tool called by the agent
- Input and output previews, latency, and outcome
- Failures, timeouts, and repeated retries
- Cross-server activity for the same actor

The wire format matches the [TypeScript SDK](https://github.com/armature-tech/mcp-analytics), so events from Go, TypeScript, and Python servers appear in the same Armature dashboards.

## How it works

Armature instruments the boundary around every tool call:

1. **InstrumentTool** adds an optional **telemetry** block to the tool’s input schema.
2. The agent can attach user intent, reasoning, and frustration to the call.
3. The SDK removes telemetry before your handler receives the arguments.
4. Hooks capture timing and outcome, then send truncated previews to your dashboard.

~~~json
{
  "telemetry": {
    "user_turn": 1,
    "user_intent": "Check whether the customer's last payment succeeded",
    "agent_thinking": "The payment lookup tool provides the requested status",
    "user_frustration": "low"
  }
}
~~~

All telemetry fields are optional. The earlier **intent**, **context**, and **frustration_level** names remain accepted for clients with cached schemas.

> **Privacy:** Armature is observability, not authentication. Keep your existing MCP authentication and authorization in place. Do not put secrets in tool arguments or telemetry fields.

## Choose the integration that matches your server

| Server shape | Integration |
| --- | --- |
| New MCP server | **NewMCPServer(...)** and **InstrumentTool(...)** |
| Existing MCP server | **NewRecorder(config)** and **server.WithHooks(rec.Hooks())** |
| Existing hooks bundle | **rec.Install(hooks)** |
| Custom tool registration | **DecorateInputSchemaWithTelemetry(...)** and **WrapHandler(...)** |

### Existing server

Start a recorder from the environment and pass its hooks to your existing
server. Set **Disabled** when the key is absent so optional analytics never
prevents the MCP server from starting:

~~~go
config := armatureanalytics.EnvConfig()
config.Disabled = config.APIKey == ""

rec, err := armatureanalytics.NewRecorder(config)
if err != nil {
    return err
}

s := server.NewMCPServer(
    "Customer MCP",
    "1.0.0",
    server.WithToolCapabilities(true),
    server.WithHooks(rec.Hooks()),
)
~~~

Close the recorder with a bounded context during shutdown to drain pending events:

~~~go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := rec.Close(ctx); err != nil {
    log.Printf("flush Armature analytics: %v", err)
}
~~~

### Existing hooks

If your server already uses hooks for OpenTelemetry or structured logging, install Armature alongside them:

~~~go
rec.Install(hooks)
~~~

### Custom registration

**InstrumentTool** combines schema decoration and handler wrapping. When you own a custom registry, use the two operations separately:

~~~go
decorated, ok := armatureanalytics.DecorateInputSchemaWithTelemetry(tool)
if ok {
    registry.Add(decorated, armatureanalytics.WrapHandler(handler))
} else {
    registry.Add(tool, handler)
}
~~~

If decoration returns **false**, register the original tool and handler. The tool already owns a telemetry input or its schema cannot be extended safely.

## Let your coding agent install it

From your MCP server repository:

~~~bash
npx --yes skills add armature-tech/mcp-analytics-go
~~~

Then ask Claude Code, Cursor, or Codex:

> Install Armature MCP Analytics using the repository’s SKILL.md. Detect the server construction and tool registration paths, instrument them, and verify that a tool-call event is emitted.

The full integration playbook is in [SKILL.md](SKILL.md).

## Configuration

**NewMCPServer** reads the standard environment variables automatically. For explicit configuration:

~~~go
config := armatureanalytics.Config{
    APIKey:      "...",
    EndpointURL: "https://app.armature.tech/api/mcp-analytics/ingest",
    Timeout:     5 * time.Second,
    ActorSeed: func(ctx context.Context) string {
        return principalFromContext(ctx)
    },
    OnError: func(err error, batch armatureanalytics.Batch) {
        log.Printf("Armature delivery failed: %v", err)
    },
}

s, shutdown := armatureanalytics.NewMCPServerWithConfig(
    "Customer MCP",
    "1.0.0",
    config,
    server.WithToolCapabilities(true),
)
~~~

| Option | Default | Purpose |
| --- | --- | --- |
| **APIKey** | **ANALYTICS_INGEST_API_KEY** with **EnvConfig** | Authenticate events and identify the MCP server |
| **EndpointURL** | Armature cloud | Override the ingestion endpoint |
| **Timeout** | 5 seconds | Set the timeout for each ingest request |
| **ActorSeed** | Anonymous | Supply a stable user or tenant seed |
| **OnError** | None | Observe delivery failures |
| **Disabled** | **false** | Disable instrumentation |

If the API key is missing, **NewMCPServer** quietly disables delivery for local
development. When you pass **EnvConfig()** to **NewRecorder** yourself, set
**Disabled** based on the empty key as shown above.

### Actor identification

**ActorSeed** should return a stable authentication principal or tenant identifier. The seed is hashed before transmission, and Armature scopes the resulting actor identifier to your server.

## What gets captured

Each **tool_call** event includes:

- Tool name, arguments preview, result preview, and error information
- Start time, finish time, and duration
- Session, client, and protocol information
- Hashed actor identifier
- Optional user intent, agent reasoning, and frustration

Each successful MCP initialization emits one deduplicated **session_init** event.

Prompts, resources, and OAuth hooks are not currently captured.

## Compatibility

- Go 1.25+
- **github.com/mark3labs/mcp-go** v0.49.0

Newer compatible mcp-go minor versions are expected to work.

## Environment variables

| Variable | Purpose |
| --- | --- |
| **ANALYTICS_INGEST_API_KEY** | Armature ingest key |
| **ANALYTICS_INGEST_URL** | Optional ingestion endpoint override |

## Example

Run the complete stdio server in [examples/minimal](examples/minimal):

~~~bash
ANALYTICS_INGEST_API_KEY="..." go run ./examples/minimal
~~~

## Support

[Open an issue](https://github.com/armature-tech/mcp-analytics-go/issues) · [Email us](mailto:hey@armature.tech) · [Changelog](CHANGELOG.md)

## License

Licensed under the [Apache License 2.0](LICENSE).
