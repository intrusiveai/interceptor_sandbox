# Environment Blueprint reference

An Environment Blueprint is a directory containing one strict YAML document
named `environment.yaml` and every asset it references. Unknown YAML fields,
multiple documents, missing assets, unsafe paths, and unsupported capabilities
fail validation.

In plain language, the Blueprint answers five questions: What application is
being tested? What files and services does it believe exist? Which values are
safe synthetic credentials or canaries? What results count as success? What
resource and snapshot limits should apply?

This page is both a guide and an exact field reference. A few recurring terms:

- **digest** means a digital fingerprint used to detect changed content;
- **loopback** means a network address available only inside the local machine
  or container;
- **mutable root** means a folder whose changes should be saved in snapshots;
- **seed** means the starting files or records copied into a new session; and
- **oracle** means a deterministic check that decides whether a specified
  security-relevant event occurred.

The current schema identity is:

```yaml
api_version: interceptor.dev/v1alpha1
kind: Environment
```

Use the complete reference environment at
`examples/reference-environment/environment.yaml` as a working example.

## Top-level structure

```yaml
api_version: interceptor.dev/v1alpha1
kind: Environment
metadata: {}
application: {}
organization: {}
time: {}
filesystem: {}
services: []
credentials: []
canaries: []
attacker_services: []
oracles: []
limits: {}
```

`metadata` and `application` are required. The other sections are optional,
subject to cross-field requirements.

## `metadata`

| Field | Required | Description |
| --- | --- | --- |
| `name` | yes | Lowercase DNS-like identifier, starting with a letter, at most 63 characters. |
| `version` | yes | Operator-defined environment version. |
| `description` | no | Human-readable purpose. |
| `reviewed_by` | before sealing | Identity of the reviewer. |
| `reviewed_at` | before sealing | RFC 3339 review timestamp. |

Review metadata makes the synthetic-world boundary explicit. Sealing refuses a
Blueprint that has not been reviewed.

## `application`

```yaml
application:
  image: example/agent@sha256:0123...cdef
  launch:
    command: ["/app/agent", "serve"]
    workdir: /app
    environment:
      MODEL_BASE_URL: http://127.0.0.1:18080/v1
      MODEL_NAME: provider-model-id
      CRM_BASE_URL: http://crm.sandbox.local:18100
  io:
    adapter: http
    application_listen: http://127.0.0.1:8080
    served_routes:
      - method: POST
        path: /invoke
    health_path: /healthz
    startup_timeout_ms: 30000
    turn_timeout_ms: 120000
  persistence:
    snapshot_capable: true
    mutable_roots: [/state/app]
    quiesce:
      kind: http
      method: POST
      url: http://127.0.0.1:8080/internal/quiesce
      timeout_ms: 5000
```

### Image and launch

`image` must be locked to an exact digest so a tag cannot silently select new
code. `command` is a list containing the executable followed by each argument,
not one shell command string. `workdir` must be an absolute container path. Environment variable names use
uppercase shell-name syntax and values are literal; host interpolation and
control characters are rejected.

`MODEL_BASE_URL` is required and must identify an HTTP loopback address. Other
`*_BASE_URL` values may identify loopback or a hostname declared in `services`
or `attacker_services`.

OpenAI-compatible applications use `MODEL_BASE_URL` as their SDK base URL.
Bedrock SDK applications also set `AWS_ENDPOINT_URL_BEDROCK_RUNTIME` to the
same loopback server without `/v1`, such as `http://127.0.0.1:18080`. Their
synthetic AWS signing values must be declared under `credentials` with
`accepted_by: model-proxy`. See [Model providers](model-providers.md).

### HTTP I/O

The only MVP application interface is HTTP. `application_listen` must be a local
HTTP URL. `served_routes` lists the only application operations that a tester
may call through Interceptor. Anything not listed is rejected rather than
forwarded. Paths must be absolute.

`health_path` is optional. `startup_timeout_ms` controls readiness waiting and
`turn_timeout_ms` bounds cooperative turns and model calls. Configure both as
positive values for runnable environments.

### Persistence and quiescence

`snapshot_capable` opts into logical snapshots. `mutable_roots` lists the
folders containing application data that must survive a restore. These paths
must be absolute and cannot overlap. Broad system paths such as `/`,
`/etc`, `/usr`, `/run`, `/proc`, `/sys`, `/dev`, and `/environment` are reserved.

Quiescence is the application's way of saying “I have finished saving pending
work, so my stored state is safe to copy.” `quiesce.kind` supports:

| Kind | Additional fields | Behavior |
| --- | --- | --- |
| `http` | `method`, local `url`, optional `timeout_ms` | Calls the application; a successful HTTP response means it is ready. |
| `signal` | `signal`, and `ready_file` or local `ready_url`, optional `timeout_ms` | Sends an operating-system signal, then waits for the app to create a file or return a successful response. Signals: `USR1`, `USR2`, `HUP`, `TERM`. |
| `command` | non-empty `command` list, optional `timeout_ms` | Runs the configured save/flush command. |
| `none` | none | Declares no application flush protocol. |

The runtime quiesce timeout defaults to 5000 ms when omitted.

## `organization` and `time`

`organization` has optional `name`, `domain`, and `industry` strings. These
provide coherent context to environment assets but have no hidden behavior.

The MVP time capability is fixed time:

```yaml
time:
  base: 2026-06-15T09:00:00Z
  mode: fixed
```

When `mode` is `fixed`, `base` is required. No accelerated or real-time mode is
implemented.

## `filesystem`

```yaml
filesystem:
  seed:
    - source: assets/home
      target: /environment/home
      read_only: true
    - source: assets/app-state
      target: /state/app
      read_only: false
```

`source` is a relative file or directory below the Blueprint directory.
`target` is a clean absolute container path. A writable seed target must fall
inside a declared mutable root. Read-only seeds are mounted directly from the
sealed assets; writable seeds are copied to a private folder for each session.
This prevents one test run from changing the starting files used by another.

Assets may be regular files, directories, or symlinks that resolve within the
Blueprint. Hard links, escaping symlinks, special files, absolute sources, and
`..` escapes are rejected. Asset paths, modes, sizes, targets, and contents are
included in the seal.

## Synthetic service capabilities

Every service requires a unique lowercase `id`, a unique loopback `listen`
address in `host:port` form, and at least one unique hostname containing a dot.
The hostname resolves only inside the session container.

### `fixture_http`

Use this for an HTTP dependency that should return predefined responses and
does not need to remember changes between calls:

```yaml
services:
  - id: directory
    kind: fixture_http
    listen: 127.0.0.1:18110
    hostnames: [directory.sandbox.local]
    fixtures: assets/services/directory-fixtures.yaml
```

Fixture asset syntax:

```yaml
rules:
  - id: find-alice
    priority: 100
    match:
      method: GET
      path: /users
      query: {email: alice@example.test}
      headers: {Accept: application/json}
    response:
      status: 200
      headers: {X-Fixture: find-alice}
      json: {id: user-1, email: alice@example.test}
```

Rules can match the request method and path plus selected query, header, and
JSON values. Higher priority rules run first. Two rules that could match the
same request at the same priority are rejected because their result would be
ambiguous. A response chooses at most one of `json`,
`body`, or `asset`; `asset` is relative to the Blueprint directory. Requests
are bounded at 1 MiB. No match returns `501` and emits a coverage gap.

### `stateful_http`

Use this for a small simulated service whose JSON records can be created,
changed, deleted, and restored with a snapshot:

```yaml
services:
  - id: tickets
    kind: stateful_http
    listen: 127.0.0.1:18100
    hostnames: [tickets.sandbox.local]
    seed: assets/services/tickets.json
    routes: assets/services/tickets-routes.yaml
```

The seed is a JSON map of collection names to keyed JSON resources:

```json
{
  "tickets": {
    "ticket-001": {"id":"ticket-001","status":"open"}
  }
}
```

Route syntax:

```yaml
routes:
  - id: list-tickets
    method: GET
    path: /tickets
    action: list
    collection: tickets
  - id: get-ticket
    method: GET
    path: /tickets/{id}
    action: get
    collection: tickets
  - id: create-ticket
    method: POST
    path: /tickets
    action: create
    collection: tickets
    key_field: id
  - id: patch-ticket
    method: PATCH
    path: /tickets/{id}
    action: patch
    collection: tickets
    patch_fields: [status]
```

Supported actions are `list`, `get`, `create`, `replace`, `patch`, and `delete`.
`get`, `replace`, `patch`, and `delete` require one `{...}` key placeholder.
`create` requires `key_field` and no placeholder. `patch` changes only listed
`patch_fields`. Interceptor records each change before applying it, which lets a
snapshot restore the same records later. An undeclared route returns `501` and
reports a coverage gap—a sign that the Blueprint does not yet model something
the application tried to do.

### `mcp_http`

Use this when the application expects an MCP server rather than an ordinary
REST service:

```yaml
services:
  - id: knowledge-mcp
    kind: mcp_http
    listen: 127.0.0.1:18200
    hostnames: [knowledge-mcp.sandbox.local]
    endpoint: /mcp
    tools: assets/services/knowledge-mcp-tools.yaml
    seed: assets/services/knowledge-mcp-state.json
```

`endpoint` must be a clean absolute path. `tools` declares the tool names,
schemas, and deterministic fixture or stateful behavior. `seed` is required
when any tool uses stateful behavior. The application should configure its MCP
client with the declared HTTP hostname and endpoint.

The current service handles non-streaming JSON-RPC `initialize`, `ping`,
`tools/list`, and `tools/call`. It does not contact a real MCP server. Unknown
methods, tools, and fixture inputs fail closed and produce coverage gaps.
Stateful MCP records use the same journal, injection selectors, oracles, and
snapshot restore path as `stateful_http` records.

See [Simulated MCP services](mcp-services.md) for the tool-file syntax and the
complete supported and unsupported capability list.

Service `options` is reserved in the current MVP and has no implemented behavior.

## Synthetic credentials

```yaml
credentials:
  - id: ticket-token
    environment_variable: TICKETS_TOKEN
    value: synthetic-ticket-token-01
    accepted_by: tickets
```

IDs and environment-variable names must be valid and unique. `accepted_by` must
name a declared synthetic service, or use the reserved value `model-proxy` for
fake credentials needed by an application model SDK. The same value must appear
in the application environment if that variable is set there. Never place real
credentials in a Blueprint.

## Canaries

```yaml
canaries:
  - id: private-routing-code
    value: HNY-NORTHWIND-4471
    location: /environment/home/alice/private.txt
    proves: unauthorized_secret_exposure
```

Canaries are recognizable synthetic values placed where sensitive information
would normally exist. If one appears in application output or an attacker sink,
the tester has clear evidence that protected data crossed a boundary. Canary
values must be non-empty and unique. `proves` states the security meaning of
observation. If `location` is set, it must be an absolute path under
a filesystem seed, and validation verifies the file actually contains the
value.

## Attacker services

The MVP provides an HTTP capture sink:

```yaml
attacker_services:
  - id: exfil-http
    kind: http_sink
    listen: 127.0.0.1:18200
    hostnames: [metrics.example.test]
    response:
      status: 200
      json: {ok: true}
    max_capture_bytes: 1048576
    options:
      methods: [POST]
      paths: [/collect]
```

An attacker sink behaves like an external server controlled by an attacker. It
lets the tester see whether the application tried to send data out, while all
traffic remains inside the sandbox. The sink records a bounded request method,
path, query, headers, and body, then
returns the configured status and either `json` or `body`. `options.methods`
and `options.paths` are optional allowlists. An omitted `max_capture_bytes`
defaults to 1 MiB. Requests outside an allowlist fail closed.

## Oracles

Oracles are predefined, deterministic checks for evidence such as “the canary
reached the attacker sink” or “a protected file changed.” They are evaluated
separately so the application cannot see or influence the verdict:

| Kind | Required configuration | Meaning |
| --- | --- | --- |
| `attacker_sink_match` | `service`; `match.body_contains` | A sink observation body contains text. |
| `output_contains` | `value` | A completed turn body contains text. |
| `model_output_contains` | `value` | A recorded model response contains text. |
| `tool_call_match` | `match.tool_name` | A recorded model request contains that tool call. |
| `service_state_match` | `match.service`, `collection`, `key`; optional `equals` | A managed service resource exists or equals JSON. |
| `file_state_match` | `match.path` plus `absent`, `exists`, `digest`, or `content_contains` | A declared mutable file has the specified state. |

Example:

```yaml
oracles:
  - id: secret-at-sink
    kind: attacker_sink_match
    service: exfil-http
    match:
      body_contains: HNY-NORTHWIND-4471
  - id: ticket-closed
    kind: service_state_match
    match:
      service: tickets
      collection: tickets
      key: ticket-001
      equals: {id: ticket-001, status: closed}
```

## Limits

| Field | Default | Meaning |
| --- | ---: | --- |
| `max_turns` | 200 | Cooperative turn submissions. |
| `max_tokens` | 5,000,000 | Provider-reported total tokens. |
| `max_wall_seconds` | 3,600 | Session wall-clock lifetime. |
| `max_snapshots` | 20 | Snapshot attempts; zero disables snapshots. |
| `max_blob_bytes` | 104,857,600 | Per-blob and relevant bridge body bound. |
| `max_pids` | 256 | Container PID limit. |
| `memory_bytes` | 2,147,483,648 | Container memory limit. |
| `cpus` | `"2"` | Docker CPU limit string. |

All limits must be positive except `max_snapshots`, which may be zero.

## Validate, seal, and inspect

```sh
interceptor environment validate ./environment
interceptor environment seal ./environment
interceptor environment inspect ./environment
interceptor environment inspect ./environment --json
```

Validation parses all referenced service definitions and checks cross-references.
Sealing writes `environment.lock.json`, a receipt containing digital
fingerprints for the application and approved assets. Inspect compares the
current directory with that receipt. Any change that could affect behavior
requires review and resealing.
