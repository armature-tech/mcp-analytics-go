# Minimal Go MCP server

This complete stdio server exposes one `echo` tool and records its tool calls with Armature.

## Run

From the repository root:

~~~bash
ANALYTICS_INGEST_API_KEY="..." go run ./examples/minimal
~~~

Launch the command from an MCP client, call `echo`, and open Armature to inspect the session.
