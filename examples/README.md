# Reference application and environment

For Python and Node applications invoked through functions, see the
[separately supplied invocation adapters](invocation-adapters/README.md).

The reference pair exercises the HTTP adapter, model proxy, stateful and MCP
services, attacker sink, both injection surfaces, and logical snapshot/restore
flow.

Build the source image, replace the Blueprint's placeholder with its exact local
image ID, then validate, seal, and build the wrapper:

```sh
docker build -t interceptor-reference-agent:local examples/reference-agent
docker image inspect --format '{{.Id}}' interceptor-reference-agent:local
# Put the printed sha256 value after `@` in reference-environment/environment.yaml.
interceptor environment validate examples/reference-environment
interceptor environment seal examples/reference-environment
interceptor app build --environment examples/reference-environment --tag interceptor-reference-sandbox:local
```

The wrapper builder recognizes a declared digest matching a local tagged image
ID, verifies the match, and uses the local tag as its build base. Registry
`name@sha256:...` references continue to work normally.

For local end-to-end testing, start the deterministic mock model provider:

```sh
go run ./examples/mock-provider
```

The mock listens on `127.0.0.1:19090` and implements
`POST /v1/chat/completions` plus `GET /healthz`.

The reference application also exposes `POST /api/mcp/smoke`. Submit a tool
name and arguments to exercise MCP initialization, discovery, and a tool call:

```json
{"tool":"search_documents","arguments":{"query":"retention"}}
```

Runtime examples use the portable file-spool bridge by default. On a native
Linux Docker host, `interceptor run` may opt into
bind-mounted Unix sockets with `--bridge-transport unix`. Do not use that mode
with Docker Desktop or a remote Docker daemon; there is deliberately no
automatic detection or fallback.

## Integrated local API lifecycle test

After starting a fresh reference environment with `interceptor run`, use
`go run ./examples/local-api-smoke --output /tmp/local-api-evidence` to qualify
attach, invocation, snapshot, restore, stale-session rejection, stop and both
session evidence exports. Add `--data-root <test-data-root>` to check the reference
application's state rollback. See [the contract](../docs/local-api.md).
