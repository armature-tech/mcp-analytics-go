---
name: install-armature-mcp-analytics-go
description: >
  Wire the mcp-analytics-go SDK into an existing Go MCP server built on
  github.com/mark3labs/mcp-go so tool calls emit telemetry to Armature. Use
  whenever the user wants to add, install, integrate, or instrument analytics
  on a Go MCP server. Detects how the server and its hooks are constructed,
  chooses the right entry point, edits the right files, and verifies both
  schema decoration and batch emission.
---

# Install mcp-analytics-go into a Go MCP server

You are integrating the `github.com/armature-tech/mcp-analytics-go` SDK into a
customer's Go MCP server. The SDK decorates each tool's input schema with an
optional `telemetry` block (`intent`, `context`, `frustration_level`), strips
that block before the handler runs, and posts an authenticated batch to
Armature after each call.

The SDK only supports servers built on `github.com/mark3labs/mcp-go`. If the
repo uses a different Go MCP framework (e.g. the official
`modelcontextprotocol/go-sdk`), stop and tell the user; do not port the server.

## Step 1: Identify the integration shape

Read enough of the repo to classify it. Grep first; only open the files you
need.

| Signal | Shape |
| --- | --- |
| The repo calls `server.NewMCPServer(...)` and you may replace that call | **A. Drop-in factory** |
| The repo calls `server.NewMCPServer(...)` with wiring you must keep (custom options builder, DI container) | **B. Recorder + hooks** |
| The repo already builds a `*server.Hooks` bundle (OTEL, logging) and passes it to `server.WithHooks` | **C. Install into existing hooks** |
| Tools are registered through a path other than `s.AddTool` (custom registry, code-gen, dispatcher) | **D. Handler wrap + schema decorate** |

Shapes B, C, and D compose: C is B with a pre-existing hooks bundle, and D
adds per-tool wiring on top of whichever of A/B/C installed the hooks.

If the repo has multiple MCP servers, ask the user which one. Do not guess.

## Step 2: Install the dependency

```bash
go get github.com/armature-tech/mcp-analytics-go
```

Requires Go 1.25+ and `github.com/mark3labs/mcp-go` v0.49.0 or newer. If the
repo pins an older mcp-go, tell the user instead of force-upgrading it.

## Step 3: Add the API key environment variable

The SDK needs one credential plus an optional URL override:

| Variable | What it is |
| --- | --- |
| `ANALYTICS_INGEST_API_KEY` | Armature ingest API key. Identifies the MCP server and signs each batch. |
| `ANALYTICS_INGEST_URL` | Optional. Defaults to `https://app.armature.tech/api/mcp-analytics/ingest`. Override for local mock or staging. |

Add `ANALYTICS_INGEST_API_KEY` to the repo's env mechanism (`.env.example`,
Docker/Kubernetes manifests, deployment docs, secret manager config). Do not
commit real secret values.

Missing-key behavior differs by entry point, and this matters:

- `NewMCPServer` / `NewMCPServerWithConfig`: no API key means no recorder is
  built and the server runs normally. Silent no-op, safe for local dev.
- `NewRecorder` directly: an empty `Config.APIKey` returns `ErrMissingAPIKey`
  unless `Config.Disabled` is true. For shape B/C, gate on the key so the
  binary still runs without analytics:

```go
cfg := armatureanalytics.EnvConfig()
cfg.Disabled = cfg.APIKey == ""
rec, err := armatureanalytics.NewRecorder(cfg)
```

## Step 4: Plan shutdown and flushing

There is no delivery-mode switch: batches always post on background goroutines
tracked by an internal `sync.WaitGroup`. What you must wire is the drain:

| Runtime | What to add |
| --- | --- |
| Long-lived process or container | `defer shutdown(ctx)` (shape A) or `defer rec.Close(ctx)` (shapes B-D) with a bounded context |
| Short-lived command / per-request serverless handler | `rec.Flush(ctx)` before the handler returns, so pending batches are not lost at process exit |

Always pass a bounded context (`context.WithTimeout`, a few seconds) so a
stuck ingest POST cannot hold up process exit. Do not call `Flush` inside
every tool handler.

## Step 5: Make the edits

In every shape, register tools through `armatureanalytics.InstrumentTool`
instead of `s.AddTool`. Hooks alone still emit `tool_call` events, but only
`InstrumentTool` (or shape D's primitives) adds the `telemetry` block to the
input schema, and that block is where intent comes from.

### Shape A: Drop-in factory

Replace the `server.NewMCPServer(...)` call; existing `server.ServerOption`s
pass through unchanged.

```go
import (
    "context"
    "time"

    "github.com/armature-tech/mcp-analytics-go/armatureanalytics"
    "github.com/mark3labs/mcp-go/mcp"
    "github.com/mark3labs/mcp-go/server"
)

s, shutdown := armatureanalytics.NewMCPServer("customer-mcp", "1.0.0",
    server.WithToolCapabilities(true),
)
defer func() {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    _ = shutdown(ctx)
}()

armatureanalytics.InstrumentTool(s,
    mcp.NewTool("lookup_customer",
        mcp.WithDescription("Look up a customer by id."),
        mcp.WithString("customer_id", mcp.Required()),
    ),
    lookupCustomerHandler,
)
```

Use `NewMCPServerWithConfig(name, version, cfg, opts...)` when the repo has
its own config layer or wants `Config.OnError` / `Config.Timeout` /
`Config.ActorSeed` set.

### Shape B: Recorder + hooks on an existing constructor

Keep the existing `server.NewMCPServer` call; add one option.

```go
cfg := armatureanalytics.EnvConfig()
cfg.Disabled = cfg.APIKey == ""
rec, err := armatureanalytics.NewRecorder(cfg)
if err != nil {
    log.Fatalf("armatureanalytics.NewRecorder: %v", err)
}
defer func() {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    _ = rec.Close(ctx)
}()

s := server.NewMCPServer("customer-mcp", "1.0.0",
    server.WithToolCapabilities(true),
    server.WithHooks(rec.Hooks()), // the added line
)
```

### Shape C: Install into an existing hooks bundle

Do not replace the customer's hooks. Add the recorder's alongside:

```go
hooks := &server.Hooks{}
hooks.AddBeforeAny(existingTracingHook)
rec.Install(hooks) // adds analytics hooks next to theirs

s := server.NewMCPServer("customer-mcp", "1.0.0", server.WithHooks(hooks))
```

### Shape D: Custom registration path or dispatcher

`InstrumentTool` is `DecorateInputSchemaWithTelemetry` + `WrapHandler` +
`s.AddTool`. When tools are registered another way, apply the two halves
yourself; the recorder's hooks (from shape A, B, or C) still do the emitting.
The decorate call returns `(tool, ok)`; when `ok` is false the tool already
declares its own `telemetry` input, so register it untouched and skip
`WrapHandler`, otherwise the handler loses a real argument.

```go
decorated, ok := armatureanalytics.DecorateInputSchemaWithTelemetry(tool)
if !ok {
    registry.Register(tool, handler)
} else {
    registry.Register(decorated, armatureanalytics.WrapHandler(handler))
}
```

For fully custom dispatch that never goes through mcp-go's handler chain, use
`armatureanalytics.WithTelemetry(ctx, tel)` / `TelemetryFromContext(ctx)` to
attach the parsed telemetry to the request context so the hooks pick it up.

### Actor identity (all shapes)

If the server authenticates users, set `Config.ActorSeed` to return the auth
principal for a request context. The SDK sha256-hashes the seed into
`actor_id`; unset means every call is recorded as the anonymous actor and
cross-session analytics degrade.

## Step 6: Verify the wiring

Two checks. Do not skip them.

**Check 1: schema includes telemetry.** List the server's tools (or unit-test
`DecorateInputSchemaWithTelemetry`). Confirm at least one tool's input schema
contains a `telemetry` property whose description mentions `intent`.

**Check 2: a real tool call produces a batch.** There is no emit stub; point
`EndpointURL` at an in-process `httptest.Server` sink, drive one call through
`mcpclient.NewInProcessClient`, then `Flush` and assert on the captured batch.

Smoke-test pattern (mirrors `integration_test/e2e_test.go` in the SDK repo):

```go
sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    body, _ := io.ReadAll(r.Body)
    var batch struct {
        Events []map[string]any `json:"events"`
    }
    _ = json.Unmarshal(body, &batch)
    mu.Lock()
    events = append(events, batch.Events...)
    mu.Unlock()
    w.WriteHeader(http.StatusAccepted)
}))
defer sink.Close()

rec, _ := armatureanalytics.NewRecorder(armatureanalytics.Config{
    APIKey:      "smoke-test-key",
    EndpointURL: sink.URL,
})
// ...build the server with rec.Hooks(), register a tool via InstrumentTool...

client, _ := mcpclient.NewInProcessClient(s)
_ = client.Start(ctx)
_, _ = client.Initialize(ctx, initReq)

callReq := mcp.CallToolRequest{}
callReq.Params.Name = "ping"
callReq.Params.Arguments = map[string]any{
    "message":   "hello",
    "telemetry": map[string]any{"intent": "verify analytics"},
}
_, _ = client.CallTool(ctx, callReq)
_ = rec.Flush(context.Background())

// Assert: one session_init event, plus a tool_call event where
// metadata.tool_name == "ping" and metadata.intent == "verify analytics".
```

A passing build or a unit test that never calls a tool is not enough. Verify
schema decoration and batch emission.

## Step 7: Mention the gotchas, then stop

Tell the user briefly:

- Which integration shape you detected.
- Where they must set `ANALYTICS_INGEST_API_KEY`.
- How shutdown/flush is wired and why (background goroutines drain on
  `shutdown` / `Close` / `Flush`).
- That a missing API key no-ops with `NewMCPServer`, and how you gated the
  recorder if you used `NewRecorder` directly.

## What NOT to do

- Do not expose `ANALYTICS_INGEST_API_KEY` to client-side code or check it
  into the repo.
- Do not leave `s.AddTool` registrations in place when `InstrumentTool` fits;
  they emit events without intent metadata and the dashboard loses the "why".
- Do not construct `NewRecorder` with an unguarded env var; an empty key
  returns `ErrMissingAPIKey` and a `log.Fatal` there turns missing analytics
  config into an outage. Gate with `Disabled` as shown in Step 3.
- Do not wrap analytics calls in recover/retry scaffolding. Delivery already
  runs off the request path, failures are swallowed, and `Config.OnError`
  exists for custom reporting.
- Do not call `Flush` in every tool handler; drain once at shutdown (or per
  request in serverless handlers).
- Do not claim full parity with the TypeScript SDK. Go covers mcp-go servers
  (factory, recorder/hooks, handler-wrap primitives); it has no Mastra-style
  adapters and no `intent: "required"` schema option yet. Function names map
  1:1 where features exist: `NewMCPServer` ↔ `createMcpAnalyticsServer`,
  `NewRecorder` ↔ `createAnalyticsRecorder`, `InstrumentTool` ↔
  `instrumentMcpServerTools`.
