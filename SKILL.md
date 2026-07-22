---
name: install-armature-mcp-analytics-go
description: >
  Install Armature MCP analytics into a Go MCP server built with either
  github.com/mark3labs/mcp-go or the official
  github.com/modelcontextprotocol/go-sdk. Use when adding, integrating, or
  instrumenting analytics in a Go MCP server. Detect the framework and server
  construction path, preserve existing hooks or middleware, decorate tool
  schemas, drain background delivery, and verify a real tool-call event.
---

# Install Armature MCP analytics in Go

Instrument a customer's Go MCP server without changing its tool-handler
behavior. The SDK adds an optional `telemetry` object to tool input schemas,
removes it before handlers run, and asynchronously sends authenticated events
to Armature.

## 1. Detect the MCP framework

Inspect `go.mod` and server construction before editing anything.

| Import | Adapter | Instructions |
| --- | --- | --- |
| `github.com/mark3labs/mcp-go` | `armatureanalytics` | Read [references/mark3labs.md](references/mark3labs.md) |
| `github.com/modelcontextprotocol/go-sdk/mcp` | `armatureanalytics/official` | Read [references/official-sdk.md](references/official-sdk.md) |

If neither import is present, stop and identify the framework; do not force an
adapter. If both are present, trace which one constructs the target server. If
the repo contains multiple MCP servers, ask which server to instrument.

Check compatibility before `go get`:

- Go 1.25.12 or newer.
- mark3labs adapter: `mcp-go` v0.49.0 or newer.
- official adapter: `modelcontextprotocol/go-sdk` v1.6.1 or newer.

Tell the user when integration requires a Go or framework upgrade. Do not
silently force-upgrade their server.

## 2. Install the matching package

Use the package path, not the module root; the module root has no Go package.

```bash
# mark3labs/mcp-go
go get github.com/armature-tech/mcp-analytics-go/armatureanalytics@latest

# official modelcontextprotocol/go-sdk
go get github.com/armature-tech/mcp-analytics-go/armatureanalytics/official@latest
```

Run `go mod tidy` after the code edits and review both `go.mod` and `go.sum`.

## 3. Configure delivery

Add the credential and region-appropriate endpoint to the repo's existing
environment and deployment mechanisms. Never commit a real value.

| Variable | Purpose |
| --- | --- |
| `ANALYTICS_INGEST_API_KEY` | Required ingest credential and server identity |
| `ANALYTICS_INGEST_URL` | Optional only for US, which defaults to `https://app.armature.tech/api/mcp-analytics/ingest`. Required for EU and must be `https://eu.armature.tech/api/mcp-analytics/ingest`. |

When the Armature dashboard supplies a copied two-line configuration, preserve
both lines; never discard `ANALYTICS_INGEST_URL` for an EU account. Add both
variables to `.env.example`, container manifests, deployment documentation, or
the repository's equivalent using placeholders. A hand-authored US
configuration may omit the URL because the SDK defaults to the US endpoint,
but keeping it explicit is recommended.

Factory helpers quietly disable analytics when the API key is absent. Direct
`NewRecorder` calls return `ErrMissingAPIKey` unless `Config.Disabled` is true
or `Config.Emit` replaces network delivery:

```go
cfg := adapter.EnvConfig()
cfg.Disabled = cfg.APIKey == ""
rec, err := adapter.NewRecorder(cfg)
```

Use the actual package name (`armatureanalytics` or `official`) instead of the
`adapter` placeholder. Preserve startup when optional analytics is unconfigured.

For production or an external pilot, set `Config.OnError`; otherwise delivery
failures are intentionally silent. If the server authenticates users, set
`Config.ActorSeed` from a stable principal in the request context. Never put
the API key in client-side code.

The network client uses a 5-second timeout per attempt and at most two
attempts, separated by 100 ms. It retries only network failures, timeouts,
`429`, and `5xx`; other `4xx` responses reach `Config.OnError` once as a
structured `*DeliveryError` without breaking the host application.

Optional verbatim identification uses `Config.ActorIdentifier`. It accepts any
non-empty string up to 8 KiB, hashes it into `actor_id`, and emits the verbatim
value only when it changes. `ActorSeed` remains the hashed-only fallback.

## 4. Choose delivery and privacy policy

Wire exactly one bounded drain:

| Runtime | Drain |
| --- | --- |
| Long-lived process | `shutdown(ctx)` from the factory, or `rec.Close(ctx)` |
| Short-lived/serverless process | `Config.Delivery = armatureanalytics.DeliveryAwait` |

Use `context.WithTimeout`; do not flush inside every tool handler. The bounded
privacy queue runs sanitization, redaction, and POSTs off the request path in
background mode; `Flush` and `Close` drain the whole pipeline.

Built-in high-confidence secret redaction is default-on. Prefer
`Config.RedactEvent` for custom whole-event policy; it may mutate or drop a
tool event. `Config.Redact` remains supported and runs first. Set
`Config.RedactSecrets` to a false pointer only when replacing the built-ins.

### Stateless HTTP / serverless sessions

When initialize and tool calls can land on different instances, call
`ResolveStatelessHTTPSession` for every parsed request. Use
`session.SessionIDGenerator()` with the official SDK's per-request
`mcp.ServerOptions.GetSessionID`, or `session.Mark3labsSessionIDManager()` with
mark3labs `server.WithSessionIdManager`. Never use mark3labs
`WithStateLess(true)` here: it intentionally returns no session ID and splits
one conversation into one analytics session per call.

The client must echo the issued `Mcp-Session-Id`. Use `DeliveryAwait` so the
serverless invocation does not freeze before ingestion finishes.
Pass the live `r.Header` map into `StatelessHTTPInput`; if the client omits its
echo, the resolver injects a one-off fallback there so the adapter preserves a
distinct request boundary. Treat these IDs as attribution, not authentication.

## 5. Verify behavior

Do all of these:

1. Run formatting, `go mod tidy`, `go vet ./...`, and the repo's tests.
2. List tools through a real in-process MCP client and confirm an instrumented
   tool schema contains optional `telemetry.user_intent`.
3. Call that tool with `telemetry.user_intent` and confirm the handler receives
   its original arguments without the top-level `telemetry` property.
4. Point `EndpointURL` at an `httptest.Server`, drain the recorder, and assert a
   `tool_call` event contains the tool name and intent. Also assert a successful
   initialization emits `session_init`.

A build-only test is insufficient. Use the framework-specific in-process
client pattern in the selected reference.

Run the language-independent local doctor against the started server too:

```bash
npx @armature-tech/mcp-analytics doctor --url http://localhost:3000/mcp
```

Use the same `ANALYTICS_INGEST_API_KEY` and `ANALYTICS_INGEST_URL` as
the Go server. The doctor verifies the MCP handshake, all served tool schemas,
and ingest authentication with an empty batch containing no customer content.
It refuses to probe when marked key, ingest, and MCP regions conflict.
Include its result in the handoff.

## 6. Report the integration

Tell the user:

- Which framework and integration shape you detected.
- Which files changed and where `ANALYTICS_INGEST_API_KEY` and the regional
  `ANALYTICS_INGEST_URL` must be configured; call out that the URL is required
  for EU.
- How shutdown/flush is bounded.
- Whether missing-key startup is gated and whether delivery errors are logged.
- Which schema, handler-cleanup, and event-emission checks passed.

## Guardrails

- Preserve existing hooks and middleware; install alongside them.
- Replace eligible tool registrations with the selected adapter's
  `InstrumentTool`; hooks or middleware alone emit calls but do not advertise
  intent telemetry.
- If a tool already owns a top-level `telemetry` input, leave it and its handler
  untouched when the decorator returns `ok == false`.
- Do not add recover/retry wrappers around analytics delivery.
- Do not claim capture of prompts, resources, OAuth, or full TypeScript feature
  parity; the Go adapters currently capture tool calls and session init.
