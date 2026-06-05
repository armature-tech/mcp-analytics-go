# mcp-analytics-go

[Armature](https://armature.tech) observability for any Go MCP server built on [`github.com/mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go).

One-line drop-in: register hooks with the MCP server at construction and every
tool call and session initialisation is captured and POSTed to Armature's
ingest endpoint.

The wire format mirrors the official TypeScript SDK
([`@armature-tech/mcp-analytics`](https://www.npmjs.com/package/@armature-tech/mcp-analytics)),
so events from Go and TS servers land in the same Armature dashboards.

## Install

```bash
go get github.com/armature-tech/mcp-analytics-go
```

## Use

```go
import (
    "context"
    "os"

    "github.com/armature-tech/mcp-analytics-go/armatureanalytics"
    "github.com/mark3labs/mcp-go/server"
)

rec, err := armatureanalytics.New(armatureanalytics.Config{
    APIKey: os.Getenv("ARMATURE_INGEST_API_KEY"),
})
if err != nil { /* handle */ }
defer rec.Close(context.Background())

s := server.NewMCPServer("my-mcp", "1.0",
    server.WithToolCapabilities(true),
    server.WithHooks(rec.Hooks()),
)
// register tools as usual...
```

That's it. Every `tools/call` produces one `tool_call` event; every successful
`initialize` produces one `session_init` event.

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

**`session_init`** — one per successful MCP `initialize`:

- `session_id_hint`, `client_name`, `client_version`, `protocol_version`
- Deduplicated per session — re-initialises do not double-emit

Other hooks (`prompts/*`, `resources/*`, OAuth) are not yet captured;
Armature's ingest schema doesn't model them.

## Configuration

```go
armatureanalytics.Config{
    APIKey:      "...",    // required; auth token for the ingest endpoint
    EndpointURL: "...",    // default: https://app.armature.tech/api/mcp-analytics/ingest
    Timeout:     5 * time.Second,                 // per-POST timeout
    ActorSeed:   func(ctx context.Context) string { /* user id / principal */ return "" },
    OnError:     func(err error, batch armatureanalytics.Batch) { /* log it */ },
    Disabled:    false,    // when true, every hook no-ops
}
```

Environment-variable convention (read by your code, passed to `Config`):

| Variable | Description |
| --- | --- |
| `ARMATURE_INGEST_API_KEY` | API key. When unset, do not call `New` — set `Disabled: true` or skip the recorder entirely. |
| `ARMATURE_INGEST_URL` | Override the ingest endpoint. |
| `ARMATURE_INGEST_TIMEOUT` | Per-POST timeout as a Go duration (e.g. `2s`). |

## Lifecycle

Tool-call timing is captured in `BeforeAny` (filtered to `tools/call`) and
completed in `OnSuccess` / `OnError`. POSTs run on a background goroutine
tracked by an internal `sync.WaitGroup`; `Flush(ctx)` and `Close(ctx)` block
until in-flight POSTs complete (or `ctx` cancels).

Once `Close` returns, the recorder is unusable — subsequent hook firings are
counted via `Dropped()` and dropped silently.

## Integrating with SigNoz MCP Server

SigNoz already has its own analytics provider interface (`pkg/analytics.Analytics`),
so the most idiomatic integration there is via a small adapter in their tree.
But the standalone hook pattern works on SigNoz too — three lines in
`internal/mcp-server/server.go` where the `*server.Hooks` is built:

```go
rec, _ := armatureanalytics.New(armatureanalytics.Config{
    APIKey:    cfg.ArmatureIngestAPIKey,
    Disabled:  cfg.ArmatureIngestAPIKey == "",
    ActorSeed: func(ctx context.Context) string {
        // derive from SigNoz's identity resolver
        if u, ok := util.GetSigNozUserID(ctx); ok { return u }
        return ""
    },
})
rec.Install(hooks) // existing *server.Hooks
defer rec.Close(ctx)
```

## Compatibility

- Go 1.22+
- `github.com/mark3labs/mcp-go` v0.49.0 (newer minor versions likely fine)

## License

Apache 2.0. See [LICENSE](LICENSE).
