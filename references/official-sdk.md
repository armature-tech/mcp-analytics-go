# Official modelcontextprotocol/go-sdk adapter

Use package:

```go
import "github.com/armature-tech/mcp-analytics-go/armatureanalytics/official"
```

The adapter uses official-SDK receiving middleware for timing and results. Its
generic `InstrumentTool` derives the official typed input schema, adds
telemetry, and preserves `mcp.ToolHandlerFor[In, Out]` signatures.

## Choose the construction shape

### Replaceable server factory

Replace `mcp.NewServer`:

```go
s, shutdown := official.NewMCPServer(
    &mcp.Implementation{Name: "customer-mcp", Version: "1.0.0"},
    serverOptions,
)
defer func() {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    _ = shutdown(ctx)
}()
```

### Existing server and middleware

Keep the server constructor and install analytics alongside existing
middleware before connecting clients:

```go
cfg := official.EnvConfig()
cfg.Disabled = cfg.APIKey == ""
rec, err := official.NewRecorder(cfg)
if err != nil {
    return err
}

s := mcp.NewServer(implementation, serverOptions)
rec.Install(s)
```

If `Config.ActorSeed` reads context values injected by another receiving
middleware, install analytics first and add that auth middleware afterward.
Across separate `AddReceivingMiddleware` calls, the later middleware executes
outermost and can pass its derived context into analytics.

### Typed tool registration

Replace eligible `mcp.AddTool` calls without changing handler signatures:

```go
official.InstrumentTool(s, &mcp.Tool{
    Name:        "lookup_customer",
    Description: "Look up a customer",
}, lookupCustomerHandler)
```

For a custom registration path:

```go
decorated, ok, err := official.DecorateInputSchemaWithTelemetry[Input](tool)
if err != nil {
    return err
}
if ok {
    mcp.AddTool(s, decorated, official.WrapHandler(handler))
} else {
    mcp.AddTool(s, tool, handler)
}
```

The typed input type must remain the handler's existing `Input` type. Do not
add analytics fields to customer structs.

## In-process verification

Use `mcp.NewInMemoryTransports()`. Run the server on one transport and connect
an `mcp.Client` to the other. `Client.Connect` performs initialization.

1. Call `session.ListTools` and inspect the selected tool's `InputSchema`.
2. Call `session.CallTool` with normal arguments plus:

```go
"telemetry": map[string]any{
    "user_turn": 1,
    "user_intent": "verify analytics",
}
```

3. In the handler, assert both the typed map input (when map-shaped) and
   `req.Params.Arguments` omit `telemetry`.
4. Drain with a bounded context and assert the recording sink received
   `session_init` and `tool_call` with the expected client, tool, and intent.
