# Changelog

All notable changes to this project will be documented in this file.

## [0.1.0] — Unreleased

### Added

- Initial `armatureanalytics` package: drop-in observability for any MCP
  server built on `github.com/mark3labs/mcp-go`.
- `armatureanalytics.AddTool(server, tool, handler)` + `WrapHandler` +
  `DecorateToolSchema` for capturing LLM-supplied `intent` / `context` /
  `frustration_level` per call. Schema-decoration is purely additive: the
  `telemetry` object is optional, never added to the schema's `required`
  list. Wire format matches the TS SDK's
  `metadata.intent` / `metadata.context` / `metadata.frustration_level`.
- `WithTelemetry` / `TelemetryFromContext` for custom registration paths
  that want to plug into the same hook machinery without using `AddTool`.
- `Recorder.Hooks()` / `Recorder.Install(*server.Hooks)` for one-line
  integration via `server.WithHooks`.
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
