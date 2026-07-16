# mark3labs/mcp-go adapter

Use package:

```go
import "github.com/armature-tech/mcp-analytics-go/armatureanalytics"
```

## Choose the construction shape

| Existing code | Integration |
| --- | --- |
| Replaceable `server.NewMCPServer(...)` | Factory |
| Constructor/options wiring must remain | Recorder + hooks |
| Existing `*server.Hooks` | Install into the hooks bundle |
| Custom tool registry | Decorate schema + wrap handler |

### Factory

```go
s, shutdown := armatureanalytics.NewMCPServer("customer-mcp", "1.0.0",
    server.WithToolCapabilities(true),
)
defer func() {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    _ = shutdown(ctx)
}()
```

Pass existing `server.ServerOption` values unchanged unless one already sets a
hooks bundle. For existing hooks, use the install shape instead of replacing
them.

### Recorder + hooks

```go
cfg := armatureanalytics.EnvConfig()
cfg.Disabled = cfg.APIKey == ""
rec, err := armatureanalytics.NewRecorder(cfg)
if err != nil {
    return err
}

s := server.NewMCPServer("customer-mcp", "1.0.0",
    server.WithToolCapabilities(true),
    server.WithHooks(rec.Hooks()),
)
```

For an existing hooks bundle:

```go
rec.Install(hooks)
s := server.NewMCPServer("customer-mcp", "1.0.0", server.WithHooks(hooks))
```

### Tool registration

Replace eligible `s.AddTool` calls:

```go
armatureanalytics.InstrumentTool(s,
    mcp.NewTool("lookup_customer",
        mcp.WithString("customer_id", mcp.Required()),
    ),
    lookupCustomerHandler,
)
```

For custom registries:

```go
decorated, ok := armatureanalytics.DecorateInputSchemaWithTelemetry(tool)
if ok {
    registry.Register(decorated, armatureanalytics.WrapHandler(handler))
} else {
    registry.Register(tool, handler)
}
```

## In-process verification

Build the server with a recorder whose `EndpointURL` is an `httptest.Server`.
Drive initialization and a tool call through
`mcpclient.NewInProcessClient(s)`. Pass:

```go
callReq.Params.Arguments = map[string]any{
    "message": "hello",
    "telemetry": map[string]any{"user_intent": "verify analytics"},
}
```

Drain with a bounded context. Assert one `session_init` and one `tool_call`
whose `metadata.tool_name` and `metadata.user_intent` match.
