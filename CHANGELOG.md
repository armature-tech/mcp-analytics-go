# Changelog

All notable changes to this project will be documented in this file.

## Unreleased

### Added

- **Per-tool telemetry hint now points agents at `request_capability`.** On a
  server that lists the SDK-owned `request_capability` tool (registered by
  `NewMCPServerWithConfig` in either package, the default once an `APIKey` or
  `Emit` is configured, or by `AddRequestCapabilityTool` on a standalone
  mark3labs server), `InstrumentTool` / `InstrumentToolWithConfig` append a
  hint that also tells agents to call `request_capability` when no other tool
  fits the user's request. Everywhere else the hint is byte-identical to
  before. Appending stays idempotent: a description already carrying any
  recognized hint is left unchanged. Owned/scrub-mode tools and the
  `request_capability` tool itself are never decorated. New exported API:
  `HintOptions`, `AppendTelemetryHintWithOptions`,
  `MarkRequestCapabilityRegistered`, `RequestCapabilityRegistered`,
  `ForgetRequestCapabilityServer`, and (official package)
  `DecorateInputSchemaWithTelemetryWithOptions`. `AppendTelemetryHint` and
  `DecorateInputSchemaWithTelemetry` keep the plain hint.

- **Tool descriptions never exceed 1024 characters because of the hint.** Some
  providers (Azure OpenAI, some OpenAI-compatible gateways) reject the whole
  request when one tool description is longer than 1024 characters. The hint
  helpers now count
  UTF-8 bytes first: when the full hint does not fit they append only its
  telemetry sentence, and when that does not fit either they leave the
  description unchanged and log a warning once per tool. Customer text is
  never truncated and the `telemetry` field is still advertised and collected.
  A description that already asks for `request_capability` gets only the
  telemetry sentence. New exported constant `MaxToolDescriptionLength`;
  `HintOptions.ToolName` names the tool in the warning.

- **`WrapStatelessHTTPHandler`: legacy-client session repair for go-sdk
  v1.7.0 stateless servers** (the Go equivalent of the TypeScript adapter's
  `wrapMcpHandler`). `StreamableHTTPHandler{Stateless: true}` on v1.7.0 mints
  no `Mcp-Session-Id` for pre-2026-07-28 clients (`ServerOptions.GetSessionID`
  is never consulted), so a legacy client — real Claude Code 2.1.x today —
  loses session attribution: its tool calls land in a heuristic fallback
  bucket with a null client, split from the initialize's `session_init`
  (verified live 2026-07-29). The middleware (on both
  `armatureanalytics.Recorder` and `official.Recorder`) detects legacy-era
  initialize POSTs, mints the identity-bearing session id via the
  `ResolveStatelessHTTPSession` scheme, attaches the `Mcp-Session-Id`
  response header, records the `session_init`, and propagates the identity to
  the wrapped per-request server through the request context
  (`StatelessHTTPSessionFromRequest`) and the internal
  `X-Armature-Stateless-Session-Id` header — a new top-of-headers rung in the
  official adapter's session-id ladder. Modern-era (2026-07-28 `_meta`
  envelope) requests pass through untouched. The official adapter's seed rung
  now also prefers an echoed `Mcp-Session-Id` that was minted from the
  request's `X-Armature-Session-Seed` (TypeScript-adapter parity), so seeded
  legacy conversations no longer split across the two headers. The Go canary
  gained an `/mcp-official` endpoint exercising this wiring end to end.

- **MCP 2026-07-28 (stateless protocol) support in the official adapter**,
  against github.com/modelcontextprotocol/go-sdk v1.7.0 (bumped from v1.6.1):
  - Per-request client identity: `clientInfo` / `protocolVersion` /
    `clientCapabilities` are captured from each request's `_meta` (typed
    `ServerRequest` accessors where available, raw `mcp.MetaKey*` slots
    otherwise). The initialize-time capture remains as the legacy-era path;
    both eras are served by one `StreamableHTTPHandler{Stateless: true}`.
    Requests without the optional `clientInfo` record an unknown client.
  - `session_init` now emits on first sight of a new session identity on any
    method (the modern era has no initialize handshake), deduplicated locally
    and by deterministic event ID at ingest.
  - Session identity ladder for `session_id_hint`: baggage
    `gen_ai.conversation.id` (from the `_meta` trace slot, W3C-decoded) →
    `X-Armature-Session-Seed` header → legacy `session.ID()` / echoed
    `Mcp-Session-Id` → process-scoped stdio ID (only when the request has no
    headers at all) → empty for server-side bucketing.
  - Tool-call events carry the request's raw `_meta` verbatim as
    `metadata.request_meta`, JSON-capped at 4 KB with a
    `request_meta_truncated` marker (`ToolCallInput.RequestMeta`,
    `MaxRequestMetaBytes`). This preserves the fixed trace-context slots
    (`traceparent`, `tracestate`, `baggage`) the Go SDK has no helpers for.
  - Telemetry schema decoration verified against v1.7.0's SEP-2106 tool
    schema validation in real `StreamableHTTPHandler` round-trips.

### Changed

- `ResolveStatelessHTTPSession` no longer mints a one-off UUID (nor injects a
  fallback `Mcp-Session-Id` header) for requests that carry neither an
  initialize message nor an echoed session ID: such requests are 2026-07-28
  traffic or a lost echo, and now resolve to an empty session that ingest
  buckets server-side instead of fabricating one session per request. The
  mint-on-initialize + echo scheme itself is retained for legacy-protocol
  hosts (mark3labs, official SDK <= v1.6); on v1.7.0 stateless servers it is
  inert because the server ignores `Mcp-Session-Id` and never consults
  `GetSessionID`.
- The official adapter snapshots tool results before handing them to the
  background privacy queue: go-sdk v1.7.0 mutates a modern-era result's
  `_meta` (server info annotation) after receiving middleware returns, which
  would otherwise race the queue's traversal.
- When a per-request or cached client capture exists but is name-less, the
  recorder now merges in the identity parsed from an identity-bearing
  stateless session ID instead of discarding it.
- The mark3labs adapter is unchanged: mark3labs/mcp-go has no 2026-07-28
  support.

- **Breaking:** `Config.RequestCapability` is now a `*bool` (was `bool`) and the
  `request_capability` tool is **on by default**. `nil` means on (once a
  delivery path is configured); set it to a pointer to `false` to opt out. A
  pointer to `true` is an explicit opt-in and is the only case where a mark3labs
  tool-name collision is surfaced via `OnError` — on by default, the customer's
  tool of the same name wins silently. Callers that set `RequestCapability:
  true` must change to a `*bool` (e.g. a pointer to `true`). The delivery-sink
  and `Disabled` gates are unchanged: no key/emit still means no injection.
- A caller-supplied `ToolCallInput.RequestID` is now scoped by `SessionID` when
  both are set (`"{sessionId}#{requestID}"`) so a transport JSON-RPC counter
  reused across concurrent conversations can no longer collide on `event_id` and
  have ingest silently drop the duplicates. Minted uuids (the default) are
  unchanged. See the "Event identity and idempotency" section of
  `packages/TELEMETRY-CONTRACT.md`.
- `Client.Send` parses the ingest 200 response body and returns an
  `*IngestRejectionError` when ingest refused events (any `rejected` entry, or
  nothing accepted from a non-empty batch), so `Config.OnError` fires instead of
  the refusal reading as success. Server-side dedup counts as accepted.

### Fixed

- Built-in secret redaction now catches AWS secret access keys written after
  their name (`AWS_SECRET_ACCESS_KEY=…`, `aws_secret_access_key = …`,
  `SecretAccessKey: …`) and replaces them with
  `[redacted:aws-secret-access-key]`, per the updated shared contract. Only
  the access key ID was caught before.
- Tool-call previews now render values that own their JSON wire format (such
  as `*mcp.CallToolResult`) through that format instead of walking raw struct
  fields, removing framework noise like `"Result":{}` and `"Annotated":{}`
  from result previews.
- Base64 payload runs are removed from previews even when embedded inside a
  larger string, closing the gap where a result's serialized text content
  retained a payload that sanitization had already removed from
  `structuredContent`.
- Sanitization work stays bounded on oversized values: string pattern rules
  scan only the budget-retainable window, and the wire-format preview path
  applies only to values estimated under 4× the sanitization budget (larger
  values keep the bounded reflective walk), so multi-megabyte tool results
  add low single-digit milliseconds rather than hundreds.

### Changed

- Removed `user_turn` from advertised schemas and newly emitted events. Cached
  clients may still send it; the SDK strips and ignores it. `UserTurn` remains
  deprecated for source compatibility.
- `agent_thinking` is requested on every call; `user_intent` and
  `user_frustration` are requested only on the first call after each new user
  message.

### Added

- **Default-on secret redaction and queued privacy processing.** Inputs,
  outputs, errors, and telemetry text now pass through bounded sanitization and
  13 high-confidence secret rules. `Config.RedactEvent` provides a context-aware
  whole-event mutate/drop hook, and a bounded FIFO queue batches delivery while
  `Flush` and `Close` drain the full pipeline.

- **Rich actor identification.** `ActorIdentifier` supplies any caller-provided
  string for both the hashed `actor_id` and verbatim identity value.
  `actor_identity` events emit only when the value changes; `ActorSeed` remains
  the hashed-only fallback.

- **Opt-in capability request tool.** `Config.RequestCapability` dynamically
  injects `request_capability` into mark3labs and official-SDK servers. The
  tool is off by default, suppressed by `Disabled` or missing delivery
  configuration, preserves the requested description exactly, and records
  provenance-marked calls through the normal analytics hooks.
- **Telemetry capture switch.** `Config.CaptureTelemetry *bool` (nil = on) plus
  `InstrumentToolWithConfig` in both packages: with capture off, tool schemas
  and descriptions pass through untouched, and telemetry sent by cached-schema
  clients is stripped and dropped at the `RecordToolCall` choke point before it
  can reach ingest or `OnError`. Shared cross-SDK behavior is specified in the
  monorepo's `packages/TELEMETRY-CONTRACT.md` with shared test vectors.
- **Telemetry field ownership in the hooks.** Tools whose schema declares a
  top-level `telemetry` property were already registered untouched; ownership
  is now also recorded (`MarkTelemetryOwnedTool` / `IsTelemetryOwnedTool`) and
  honored by the mark3labs recorder hooks and the official-SDK middleware, so
  a customer-owned `telemetry` argument is never interpreted as Armature
  telemetry and stays visible to the tool and its input preview. A collision
  warning is logged once per tool. `Config.TelemetryFieldMap` opts specific
  customer fields into export explicitly (read, never stripped).
- **Preview sanitization and redaction.** Image/audio content-block `data`,
  resource `blob`s, base64 data URIs, and long base64-only strings are replaced
  with contract placeholders before previews are serialized. `Config.Redact`
  runs over the sanitized inputs/outputs, error strings, and telemetry text
  (sanitize → redact → stringify → truncate); a panicking hook fails closed to
  `[redaction failed]` while the event still ships.
- Cross-language stateless HTTP helpers: `ResolveStatelessHTTPSession`,
  identity-bearing session IDs, official-SDK session generators, and a
  mark3labs request-scoped session manager.
- `DeliveryAwait` and custom `Config.Emit` delivery for serverless functions
  and custom pipelines.
- Workflow-run markers derived from `X-Armature-Workflow-Run-Id`, plus the
  Authorization-header actor fallback used by the TypeScript and Python SDKs.
- Process-scoped stdio session IDs, lazy `session_init` on cold tool-call
  instances, bounded session dedup, and client identity recovery from
  stateless session IDs.
- Explicit `ToolCallInput.RequestID`; otherwise each call receives a fresh UUID
  so reconnecting JSON-RPC counters cannot collide at ingest.
- Official `github.com/modelcontextprotocol/go-sdk/mcp` support through the
  `armatureanalytics/official` adapter. It provides receiving middleware,
  factory and recorder integration, typed `InstrumentTool`, schema decoration,
  handler cleanup, session initialization, and end-to-end tests.
- Framework-neutral `Recorder.RecordToolCall` and `RecordSessionInit` entry
  points for additional adapters.
- A complete official-SDK stdio example and framework-specific skill
  references.

### Fixed

- Sessionless official-SDK requests no longer share an empty metadata-cache
  key, and requests without an evictable server session are not retained.
- Cancelled mark3labs calls that never reach a completion hook now emit a
  cancelled tool event and release their pending arguments.
- Pending-call registration is serialized with recorder shutdown, preventing
  calls from being retained after `Close` performs its cleanup sweep.
- Installation commands now target importable package paths instead of the
  package-less module root, so clean consumer builds receive transitive sums.
- CI and the module minimum now use patched Go 1.25.12 rather than Go 1.25.5.
- Both stdio examples return through their bounded analytics drain before
  `log.Fatal` handles a server error.

## [0.1.5] — 2026-07-15

### Changed

- **V1 telemetry schema.** The injected `telemetry` parameter moves to the V1
  field names: `user_intent` (was `intent`), `agent_thinking` (was `context`),
  `user_frustration` (was `frustration_level`), plus the new `user_turn`
  (1-based count of user messages, repeated on every call). Description
  strings are byte-identical with the TS and Python SDKs. Legacy input
  spellings are still accepted and normalized (V1 wins on conflict;
  fractional/zero/negative `user_turn` values are dropped, not coerced), and
  emitted metadata carries the V1 keys plus legacy mirrors so a
  not-yet-updated ingest keeps reading events from this SDK.
- **Description nudge.** `InstrumentTool` now appends the telemetry hint to
  each tool description (idempotently), matching the TS/Python SDKs — the Go
  SDK previously injected the schema without the nudge, so agents were far
  less likely to fill in intent. `AppendTelemetryHint` is exported for custom
  registration paths.

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
