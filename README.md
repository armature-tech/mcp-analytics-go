# Armature MCP Analytics for Go

Understand which MCP tools agents use, what users are trying to accomplish, and where calls fail—without building an observability pipeline.

[![Go Reference](https://pkg.go.dev/badge/github.com/armature-tech/mcp-analytics-go.svg)](https://pkg.go.dev/github.com/armature-tech/mcp-analytics-go/armatureanalytics)
[![CI](https://github.com/armature-tech/mcp-analytics-go/actions/workflows/ci.yml/badge.svg)](https://github.com/armature-tech/mcp-analytics-go/actions/workflows/ci.yml)
[![GitHub release](https://img.shields.io/github/v/release/armature-tech/mcp-analytics-go)](https://github.com/armature-tech/mcp-analytics-go/releases)
[![Apache 2.0](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

[Armature](https://armature.tech) · [TypeScript SDK](https://github.com/armature-tech/mcp-analytics) · [Python SDK](https://github.com/armature-tech/mcp-analytics-python) · [Agent install](SKILL.md)

Built for Go MCP servers using either
[mark3labs/mcp-go](https://github.com/mark3labs/mcp-go) or the official
[modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk).

## Install in 30 seconds

### 1. Install

Choose the package matching your MCP framework:

~~~bash
# mark3labs/mcp-go
go get github.com/armature-tech/mcp-analytics-go/armatureanalytics@latest

# official modelcontextprotocol/go-sdk
go get github.com/armature-tech/mcp-analytics-go/armatureanalytics/official@latest
~~~

### 2. Add your ingest key

Create a server in the [Armature dashboard](https://app.armature.tech), copy its ingest key, and add it to your environment:

~~~bash
export ANALYTICS_INGEST_API_KEY="..."
~~~

### 3. Instrument your MCP server

#### mark3labs/mcp-go

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

#### Official modelcontextprotocol/go-sdk

The official adapter preserves the SDK's typed handler signature:

~~~go
import (
    "context"
    "time"

    "github.com/armature-tech/mcp-analytics-go/armatureanalytics/official"
    "github.com/modelcontextprotocol/go-sdk/mcp"
)

type LookupInput struct {
    CustomerID string `json:"customer_id" jsonschema:"customer id"`
}

type LookupOutput struct {
    Active bool `json:"active"`
}

s, shutdown := official.NewMCPServer(
    &mcp.Implementation{Name: "Customer MCP", Version: "1.0.0"},
    nil,
)
defer func() {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    _ = shutdown(ctx)
}()

official.InstrumentTool(s, &mcp.Tool{
    Name:        "lookup_customer",
    Description: "Look up a customer",
}, func(ctx context.Context, req *mcp.CallToolRequest, input LookupInput) (
    *mcp.CallToolResult,
    LookupOutput,
    error,
) {
    return nil, LookupOutput{Active: true}, nil
})
~~~

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
    "user_intent": "Check whether the customer's last payment succeeded",
    "agent_thinking": "The payment lookup tool provides the requested status",
    "user_frustration": "low"
  }
}
~~~

All telemetry fields are optional. Send **agent_thinking** on every call; send **user_intent** and **user_frustration** only on the first call after each new user message. Their absence on later calls means the same turn continues. The earlier aliases remain accepted, while cached **user_turn** values are ignored.

> **Privacy:** Armature is observability, not authentication. Keep your existing MCP authentication and authorization in place. Do not put secrets in tool arguments or telemetry fields.

## Choose the integration that matches your server

| Framework / server shape | Integration |
| --- | --- |
| mark3labs, new server | `armatureanalytics.NewMCPServer(...)` and `InstrumentTool(...)` |
| mark3labs, existing server | `NewRecorder(config)` and `server.WithHooks(rec.Hooks())` |
| mark3labs, existing hooks bundle | `rec.Install(hooks)` |
| Official SDK, new server | `official.NewMCPServer(...)` and `official.InstrumentTool(...)` |
| Official SDK, existing server | `official.NewRecorder(config)`, then `rec.Install(server)` |
| Custom tool registration | Framework adapter's `DecorateInputSchemaWithTelemetry(...)` and `WrapHandler(...)` |
| Stateless HTTP / serverless | `ResolveStatelessHTTPSession(...)` per request |

### Stateless HTTP and serverless

Initialization and tool calls can land on different instances. Resolve every
request before constructing its per-request MCP server/transport:

~~~go
var body any
raw, _ := io.ReadAll(r.Body)
_ = json.Unmarshal(raw, &body)
r.Body = io.NopCloser(bytes.NewReader(raw))

session := armatureanalytics.ResolveStatelessHTTPSession(
    armatureanalytics.StatelessHTTPInput{Body: body, Headers: r.Header},
)
~~~

For the official SDK, set the initialize-only generator on the per-request
server and keep the transport stateless:

~~~go
s, shutdown := official.NewMCPServer(
    &mcp.Implementation{Name: "Customer MCP", Version: "1.0.0"},
    &mcp.ServerOptions{GetSessionID: session.SessionIDGenerator()},
)
defer shutdown(r.Context())

handler := mcp.NewStreamableHTTPHandler(
    func(*http.Request) *mcp.Server { return s },
    &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
)
handler.ServeHTTP(w, r)
~~~

For mark3labs, pass the request-scoped no-store manager:

~~~go
handler := server.NewStreamableHTTPServer(
    s,
    server.WithSessionIdManager(session.Mark3labsSessionIDManager()),
)
handler.ServeHTTP(w, r)
~~~

The helper mints `mcp_<client>_v_<version>_<uuid>` at `initialize`; compliant
clients echo it in `Mcp-Session-Id`, so later cold invocations recover the same
session and client identity without shared storage. If a later request omits
the echo, the helper injects a one-off fallback into the supplied request
headers so the framework adapter still records a distinct request boundary.
Treat the ID as
observability, never authentication. Set `Config.Delivery` to
`armatureanalytics.DeliveryAwait` in serverless handlers.

### Existing mark3labs server

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

### Existing official-SDK server

Install receiving middleware without replacing the server or its existing
middleware:

~~~go
config := official.EnvConfig()
config.Disabled = config.APIKey == ""

rec, err := official.NewRecorder(config)
if err != nil {
    return err
}
rec.Install(server)
~~~

Register typed tools through `official.InstrumentTool` instead of
`mcp.AddTool`. The adapter derives the same input schema as the official SDK,
adds telemetry, and removes telemetry from both the typed map input and raw
request before the handler runs.

### Custom mark3labs registration

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
    Delivery:    armatureanalytics.DeliveryAwait,
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

The official adapter accepts the same `Config` fields through
`official.NewMCPServerWithConfig(implementation, serverOptions, config)`.

| Option | Default | Purpose |
| --- | --- | --- |
| **APIKey** | **ANALYTICS_INGEST_API_KEY** with **EnvConfig** | Authenticate events and identify the MCP server |
| **EndpointURL** | Armature cloud | Override the ingestion endpoint |
| **Timeout** | 5 seconds | Set the timeout for each ingest request |
| **Delivery** | `DeliveryBackground` | Use `DeliveryAwait` for serverless and short-lived handlers |
| **Emit** | Network emitter | Replace delivery for tests or custom pipelines; makes APIKey optional |
| **ActorSeed** | Authorization header, then anonymous | Supply a stable user or tenant seed |
| **OnError** | None | Observe delivery failures |
| **Disabled** | **false** | Disable instrumentation |
| **CaptureTelemetry** | **nil** (on) | Disable conversation-derived telemetry entirely (see below) |
| **Redact** | None | Redact sensitive data from previews before delivery (see below) |
| **TelemetryFieldMap** | None | Export existing argument fields as telemetry (see below) |

### Telemetry capture and privacy

`InstrumentTool` injects an optional `telemetry` object (`user_intent`, `agent_thinking`, `user_frustration`) into each wrapped tool's input schema. This is conversation-derived data: if your deployment cannot disclose it — for example in a privacy policy required for an app-store submission — set **CaptureTelemetry** to a false pointer and register tools through `InstrumentToolWithConfig` (both packages provide it). With capture off, tool schemas and descriptions pass through completely untouched, and telemetry sent by clients holding an older cached schema is stripped and never delivered anywhere (ingest or `OnError`). Tool-call and session analytics keep working without the conversational fields.

Disclosure summary for privacy policies: with capture **on**, the SDK collects tool names, tool call inputs/outputs (size-capped previews), error messages, timing, a one-way hash of the actor seed, client name/version, and the agent-supplied `telemetry` fields above; recipients are your Armature workspace. With capture **off**, the `telemetry` fields are not collected.

If a tool's own input schema already declares a top-level `telemetry` property, the SDK treats that field as **yours**: the schema, description, and arguments pass through untouched (including in the recorder hooks and middleware), nothing is interpreted as Armature telemetry, and a warning is logged once at registration. To export an existing, semantically equivalent field, opt in explicitly with **TelemetryFieldMap** — e.g. `map[string]string{"user_intent": "purpose"}` reads (never strips) the tool's `purpose` argument into `user_intent`. Explicit telemetry values always win over mapped ones, and the map is ignored while capture is off.

### Redaction and binary payloads

Before any preview is serialized, the SDK strips binary content automatically: image/audio content-block `data`, resource `blob`s, base64 data URIs, and long base64 strings are replaced with `"[binary removed]"` / `"[base64 removed]"` placeholders. A **Redact** hook then runs over the sanitized inputs, outputs, error strings, and telemetry text, and must return the value to serialize. The pipeline is sanitize → redact → stringify → truncate. If the hook panics, the SDK fails closed: the affected payload is replaced with `"[redaction failed]"` and the event still ships.

If the API key is missing, **NewMCPServer** quietly disables delivery for local
development. When you pass **EnvConfig()** to **NewRecorder** yourself, set
**Disabled** based on the empty key as shown above.

For production and external pilots, set `OnError`; otherwise an invalid key or
ingest failure is intentionally silent.

### Actor identification

**ActorSeed** should return a stable authentication principal or tenant identifier. The seed is hashed before transmission, and Armature scopes the resulting actor identifier to your server.

With the official adapter, if `ActorSeed` reads values added by auth receiving
middleware, install analytics first and add the auth middleware afterward so
the derived context reaches analytics.

## What gets captured

Each **tool_call** event includes:

- Tool name, arguments preview, result preview, and error information
- Start time, finish time, and duration
- Session, client, and protocol information
- Hashed actor identifier
- Optional user intent, agent reasoning, and frustration

Each successful MCP initialization emits one deduplicated **session_init** event.
On a cold stateless tool-call instance, the recorder lazily re-emits the same
stable session event; ingest coalesces it by event ID. Stdio servers receive a
process-scoped session ID so separate CLI conversations never merge.

Prompts, resources, and OAuth hooks are not currently captured.

## Compatibility

- Go 1.25.12+
- **github.com/mark3labs/mcp-go** v0.49.0 through v0.56.0
- **github.com/modelcontextprotocol/go-sdk** v1.6.1

The CI suite tests the declared minimum mark3labs version; a compatibility leg
also tests the current v0.56 line.

## Environment variables

| Variable | Purpose |
| --- | --- |
| **ANALYTICS_INGEST_API_KEY** | Armature ingest key |
| **ANALYTICS_INGEST_URL** | Optional ingestion endpoint override |

## Example

Run either complete stdio example:

~~~bash
ANALYTICS_INGEST_API_KEY="..." go run ./examples/minimal
ANALYTICS_INGEST_API_KEY="..." go run ./examples/official
~~~

## Support

[Open an issue](https://github.com/armature-tech/mcp-analytics-go/issues) · [Email us](mailto:hey@armature.tech) · [Changelog](CHANGELOG.md)

## License

Licensed under the [Apache License 2.0](LICENSE).
