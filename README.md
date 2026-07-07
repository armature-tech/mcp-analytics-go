# mcp-analytics-go

[Armature](https://armature.tech) analytics for any Go MCP server built on [`github.com/mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go) — drop in a recorder, get a dashboard of who's calling your tools, what they're asking for, and where they're getting stuck. On Armature you can see:

- Who your users are and which tools they actually use
- What agents are *trying* to accomplish (user intent, agent thinking, frustration captured per call)
- Where tools fail, time out, or get retried
- Cross-server activity for the same user, even across vendors

Wire format matches the official TypeScript SDK ([`@armature-tech/mcp-analytics`](https://github.com/armature-tech/mcp-analytics)), so events from Go and TS servers land in the same Armature dashboards.

## Getting Started

**Cloud:** sign in at [app.armature.tech](https://app.armature.tech), create a server, copy the API key.

**Install the SDK:**

```bash
go get github.com/armature-tech/mcp-analytics-go
```

**Wrap your server** — the one-line shape:

```go
import (
    "context"
    "time"

    "github.com/armature-tech/mcp-analytics-go/armatureanalytics"
    "github.com/mark3labs/mcp-go/mcp"
    "github.com/mark3labs/mcp-go/server"
)

s, shutdown := armatureanalytics.NewMCPServer("my-mcp", "1.0.0",
    server.WithToolCapabilities(true),
)
defer func() {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    _ = shutdown(ctx)
}()

armatureanalytics.InstrumentTool(s,
    mcp.NewTool("echo", mcp.WithDescription("Echoes"), mcp.WithString("text")),
    echoHandler,
)
```

That's it. Every tool registered through `armatureanalytics.InstrumentTool` is now instrumented. Open the dashboard and the first tool call shows up.

`NewMCPServer` reads `ANALYTICS_INGEST_API_KEY` from the environment. When it's unset the recorder no-ops, so the same binary runs with and without analytics enabled.

## Why mcp-analytics

**1) Generic analytics don't understand MCP.**

An MCP tool call has structure that page-view analytics throws away: the tool name, the args the agent constructed, whether the call succeeded, what the agent was *trying to do*. You want those as first-class fields, not buried in custom dimensions.

**2) Instrumenting by hand is the same boilerplate every time.**

Decorate input schemas, strip telemetry fields before the handler runs, time the call, batch, retry, dedupe sessions, propagate auth. Every MCP server reinvents it. This package is that boilerplate, packaged once.

**3) The agent should be able to tell you what it's doing.**

`InstrumentTool` adds an optional `telemetry` object to each tool's input schema with `user_turn`, `user_intent`, `agent_thinking`, and `user_frustration`. Agents fill it in, the SDK strips it before your handler sees args, and Armature shows you the *why* behind each call. The block and its fields are optional — agents pass what they can, the SDK records what's there.

## How it works

Three things happen on every tool call:

1. **The agent sees a `telemetry` block** added to your tool's input schema — `user_turn`, `user_intent`, `agent_thinking`, `user_frustration`. The block is optional; the SDK never rejects a call for omitting it. (Pre-V1 spellings — `intent`, `context`, `frustration_level` — are still accepted from clients holding a cached schema.)
2. **Your handler sees its original args.** The SDK strips `telemetry` before invoking it.
3. **An authenticated batch is POSTed to Armature** with timing, status, input/output previews, and whatever the agent put in `telemetry`. The first call on a new `sessionId` is preceded by a `session_init` event.

Hook timing is captured in `BeforeAny` (filtered to `tools/call`) and completed in `OnSuccess` / `OnError`. POSTs run on background goroutines tracked by an internal `sync.WaitGroup`; the `shutdown` returned by `NewMCPServer` (or `Recorder.Flush` / `Close`) drains them before process exit.

## Other integration shapes

`NewMCPServer` covers most servers. If yours doesn't fit, three lower-level entry points let you wire the recorder in by hand:

- **Existing `*server.MCPServer`** — `armatureanalytics.NewRecorder(cfg)` returns a recorder, `rec.Hooks()` returns hooks you pass to `server.WithHooks(...)`. Use when you already construct the server yourself and want to keep that wiring. Mirrors the TS SDK's `createAnalyticsRecorder`.
- **Existing `*server.Hooks` bundle** — `rec.Install(hooks)` adds the recorder's hooks alongside yours (OTEL, structured logging, etc.) without replacing them. Use when you already register other hooks at construction time.
- **Custom tool dispatcher** — `armatureanalytics.WrapHandler(handler)` + `DecorateInputSchemaWithTelemetry(tool)` are the two halves of `InstrumentTool`. Use when you register tools through a path other than `s.AddTool` (custom registries, code-gen) and want to keep telemetry capture. `DecorateInputSchemaWithTelemetry` returns `(tool, ok)`: when `ok` is false the tool already declares its own `telemetry` input — register it untouched and skip `WrapHandler`, otherwise the handler loses a real argument. Mirrors the TS SDK's `decorateInputSchemaWithTelemetry`.

`WithTelemetry` / `TelemetryFromContext` let custom paths attach a `Telemetry` value to the request context so the recorder's hooks pick it up regardless of how the tool was registered.

## Configuration

```go
armatureanalytics.Config{
    APIKey:      "...",    // required; auth token for the ingest endpoint
    EndpointURL: "...",    // default: https://app.armature.tech/api/mcp-analytics/ingest
    Timeout:     5 * time.Second, // per-POST timeout
    ActorSeed:   func(ctx context.Context) string { /* user id / principal */ return "" },
    OnError:     func(err error, batch armatureanalytics.Batch) { /* log it */ },
    Disabled:    false,    // when true, every hook no-ops
}
```

**Actor id.** `ActorSeed(ctx)` returns the actor seed (typically the auth principal). The SDK hashes it with sha256 into the `actor_id` on the wire. Armature scopes actor ids to your server via the API key, so the same seed under two servers stays linked to the same person (cross-surface analytics). When unset or empty, the actor is recorded as `sha256("anonymous")`.

**Missing API key.** The SDK no-ops — every hook is a no-op, no network calls are made. Useful for local development and for the same binary running with/without analytics.

**Auth.** Each batch is POSTed with `Authorization: Bearer <apiKey>`. Server identity is resolved from the API key — no separate header.

## Environment variables

| Variable | Purpose |
| --- | --- |
| `ANALYTICS_INGEST_API_KEY` | Your Armature API key — identifies the MCP server and signs each batch. When unset, the recorder no-ops. |
| `ANALYTICS_INGEST_URL` | Ingest endpoint (defaults to `https://app.armature.tech/api/mcp-analytics/ingest`; override for a local mock or staging). |

These are read by `armatureanalytics.EnvConfig()` and `NewMCPServer`. If you build a `Config` by hand, read them yourself.

## What gets captured

**`tool_call`** — one per MCP tool invocation:

- `tool_name` — `request.Params.Name`
- `args` — JSON-stringified `request.Params.Arguments`, truncated to 8 KiB
- `result` — JSON preview of the tool's return value, truncated to 8 KiB
- `started_at` / `finished_at` / `duration_ms`
- `ok` — `true` when the handler returned no error and `result.IsError` was false
- `error` — error type (Go error string, or `errorType` if supplied) on failure
- `session_id_hint` — current `ClientSession.SessionID()`
- `client_name` / `client_version` / `protocol_version` — captured at `initialize`
- `actor_id` — sha256 of `Config.ActorSeed(ctx)`; defaults to sha256("anonymous")
- `metadata.intent` / `metadata.context` / `metadata.frustration_level` — populated when the agent fills the `telemetry` block

**`session_init`** — one per successful MCP `initialize`:

- `session_id_hint`, `client_name`, `client_version`, `protocol_version`
- Deduplicated per session — re-initialises do not double-emit

Other hooks (`prompts/*`, `resources/*`, OAuth) are not yet captured; Armature's ingest schema doesn't model them.

## Compatibility

- Go 1.25+
- `github.com/mark3labs/mcp-go` v0.49.0 (newer minor versions likely fine)

## More

- **Example** — [`examples/minimal`](examples/minimal) — minimal stdio MCP server with the recorder wired in.
- **Lower-level primitives** — `NewRecorder`, `Hooks`, `Install`, `WrapHandler`, `DecorateInputSchemaWithTelemetry`, `WithTelemetry`, `TelemetryFromContext` are exported for cases the shapes above don't cover.
- **Name parity with the TS SDK.** Public function names map across SDKs so the same docs apply: `NewMCPServer` ↔ `createMcpAnalyticsServer`, `NewRecorder` ↔ `createAnalyticsRecorder`, `InstrumentTool` ↔ `instrumentMcpServerTools` (per-tool), `DecorateInputSchemaWithTelemetry` ↔ `decorateInputSchemaWithTelemetry`, `Recorder.Flush` ↔ `recorder.flush`.
- **Support** — `hey@armature.tech` or open an issue.

## License

Apache 2.0. See [LICENSE](LICENSE).
