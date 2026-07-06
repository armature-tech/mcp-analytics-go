# Changelog

All notable changes to this project will be documented in this file.

## [0.1.0] — Unreleased

### Changed

- `DecorateInputSchemaWithTelemetry` now returns `(mcp.Tool, bool)`. The
  bool reports whether the schema was decorated, so custom registration
  paths can skip `WrapHandler` when a tool declares its own `telemetry`
  input (or when a raw schema cannot be extended).

### Fixed

- String-encoded `telemetry` arguments no longer drop the tool's sibling
  arguments during extraction.
- `InstrumentTool` and `DecorateInputSchemaWithTelemetry` leave tools that
  declare their own top-level `telemetry` input untouched instead of
  stripping a real argument before the handler runs.
- Pending tool-call state and `session_init` dedup are keyed per client
  session even when the transport reports an empty session id, so concurrent
  sessionless connections no longer collide on shared JSON-RPC ids or emit
  duplicate `session_init` events on re-initialize.
- Tool-call completions that land after `Close` clean up their pending-call
  state instead of leaking it.
- `decorateToolSchema` no longer mutates the caller's `Properties` map; the
  decorated copy gets its own map, as the docs promised.
- A completion racing `Close` can no longer start an ingest POST after
  `Close` has returned: the closed transition and in-flight registration
  are serialized, so `Close` drains everything that got in and drops the
  rest.
- `tool_call` events on sessionless transports now carry `client_name` /
  `client_version` / `protocol_version`; client info is tracked by the
  per-connection session key instead of the (empty) session id string.
- `examples/minimal` drains in-flight analytics before exiting on
  SIGINT/SIGTERM (`os.Exit` skips deferred functions).

### Added

- Initial `armatureanalytics` package: drop-in observability for any MCP
  server built on `github.com/mark3labs/mcp-go`.
- `armatureanalytics.InstrumentTool(server, tool, handler)` + `WrapHandler`
  + `DecorateInputSchemaWithTelemetry` for capturing LLM-supplied `intent`
  / `context` / `frustration_level` per call. Schema-decoration is purely
  additive: the `telemetry` object is optional, never added to the
  schema's `required` list. Wire format matches the TS SDK's
  `metadata.intent` / `metadata.context` / `metadata.frustration_level`.
- Public function names map to the TS SDK's exports for parity across
  SDKs: `NewMCPServer` ↔ `createMcpAnalyticsServer`, `NewRecorder` ↔
  `createAnalyticsRecorder`, `InstrumentTool` ↔ `instrumentMcpServerTools`
  (per-tool), `DecorateInputSchemaWithTelemetry` ↔
  `decorateInputSchemaWithTelemetry`.
- `WithTelemetry` / `TelemetryFromContext` for custom registration paths
  that want to plug into the same hook machinery without using `AddTool`.
- `Recorder.Hooks()` / `Recorder.Install(*server.Hooks)` for one-line
  integration via `server.WithHooks`.
- `NewMCPServer(name, version, opts...)` / `NewMCPServerWithConfig` —
  construct a `*server.MCPServer` with analytics pre-wired from the
  environment. Returns a `Shutdown(ctx)` that flushes pending batches.
- Environment variables `ANALYTICS_INGEST_API_KEY` /
  `ANALYTICS_INGEST_URL` (read by `EnvConfig` and `NewMCPServer`),
  matching the TS SDK's published convention so the same env names
  work across both SDKs.
- Telemetry field descriptions on the decorated input schema match the
  TS SDK's canonical strings verbatim, so the LLM sees the same intent /
  context / frustration_level guidance regardless of which SDK is in use.
- `tool_call` events on every `tools/call` (captured via `BeforeAny` +
  `OnSuccess` / `OnError` filtered to that method).
- `session_init` events on successful `initialize`, captured via
  `AfterInitialize` and deduplicated per session.
- `Recorder.Flush(ctx)` and `Recorder.Close(ctx)` drain in-flight ingest
  POSTs via an internal `sync.WaitGroup`.
- `Config.OnError` hook for surfacing ingest delivery failures.
- `Config.Disabled` flag for env-var opt-out without restructuring call sites.
- `examples/minimal` — minimal stdio MCP server with the recorder wired in.
- Wire-format parity with `@armature-tech/mcp-analytics` (TS SDK):
  `schema_version: 1`, sha256 actor/event IDs, Bearer auth, identical
  `tool_call` and `session_init` Event shapes.

### Tested

- Unit tests for event translation, UTF-8-safe truncation, anonymous actor
  fallback, error-path classification.
- Ingest client tests against `httptest.Server`: payload shape, 2xx vs 4xx,
  timeout, User-Agent / Authorization headers.
- End-to-end integration test driving a real in-process `mcp-go` server with
  successful and failing tool calls, verifying session_init + tool_call
  events land at the recording sink with correct metadata.
