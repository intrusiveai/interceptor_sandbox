# Environment Blueprint reference

An Environment Blueprint is a directory containing one strict YAML document
named `environment.yaml` and every asset it references. Unknown YAML fields,
multiple documents, missing assets, unsafe paths, and unsupported capabilities
fail validation.

For `application.interface.kind: code`, optional `code.adapter_asset` names a
directory within this Blueprint that `app build` installs into the wrapper.
The existing `code.module` selects its Python module or explicit relative Node
entry file. All adapter files are sealed assets; changing them requires resealing
and rebuilding. Optional `code.close` names a cleanup method on the initialized
object (or module without initialization). See the
[adapter configuration and lifecycle](application-interfaces.md#supplying-an-adapter-separately)
for path restrictions, dependencies, and examples.

In plain language, the Blueprint answers six questions: What application is
being tested? What files and services does it believe exist? How should its
closed DNS and HTTP/TLS plane behave? Which values are safe synthetic
credentials or canaries? What results count as success? What resource and
snapshot limits should apply?

This page is both a guide and an exact field reference. A few recurring terms:

- **digest** means a digital fingerprint used to detect changed content;
- **loopback** means a network address available only inside the local machine
  or container;
- **mutable root** means a folder whose changes should be saved in snapshots;
- **seed** means the starting files or records copied into a new session; and
- **oracle** means a deterministic check that decides whether a specified
  security-relevant event occurred.

The original schema identity (still supported) is:

```yaml
api_version: interceptor.dev/v1alpha1
kind: Environment
```

Use the complete reference environment at
`examples/reference-environment/environment.yaml` as a working example.
For application `delivery` / `delivery_asset`, service `export_contracts`
or MCP `implementation`, select `api_version: interceptor.dev/v1alpha2`.
See [Delivery contracts](delivery-contracts.md) for these exact fields and the
matching versioned capability export. Source seeds stay immutable; fidelity file
writes affect only the session copy.

## Top-level structure

```yaml
api_version: interceptor.dev/v1alpha1
kind: Environment
metadata: {}
application: {}
organization: {}
time: {}
network: {}
filesystem: {}
services: []
identity: {}
credentials: []
canaries: []
attacker_services: []
oracles: []
limits: {}
adaptive: {}
```

`metadata` and `application` are required. The other sections are optional,
subject to cross-field requirements.

## `adaptive`

```yaml
adaptive:
  feedback_profiles: [black-box, diagnostic]
  max_attempts: 200
  max_artifacts: 400
```

`feedback_profiles` may contain `black-box`, `diagnostic`, and
`oracle-assisted`. A session selects exactly one supported profile and cannot
broaden it later. Attempt and artifact counts are bounded from 1 through their
implementation ceilings; defaults are 200 and 400. This section contains no
planner, AEF, Operator, credential-store, or payload content.

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
  interface:
    kind: http
    operations:
      - id: invoke
        method: POST
        path: /invoke
    http:
      listen: http://127.0.0.1:8080
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
`*_BASE_URL` values may identify loopback or a hostname declared in `services`,
`identity.issuers`, or `attacker_services`. An HTTPS endpoint must use a
declared hostname and the port configured by `network.tls.listen`.

OpenAI-compatible applications use `MODEL_BASE_URL` as their SDK base URL.
Bedrock SDK applications also set `AWS_ENDPOINT_URL_BEDROCK_RUNTIME` to the
same loopback server without `/v1`, such as `http://127.0.0.1:18080`. Their
synthetic AWS signing values must be declared under `credentials` with
`accepted_by: model-proxy`. See [Model providers](model-providers.md).

## `network`

The optional closed network simulation replaces hostname shortcuts with a
recorded DNS resolver and adds shared HTTP/HTTPS entry points:

```yaml
network:
  dns:
    enabled: true
    listen: 127.0.0.1:53
    unknown_policy: nxdomain
    max_queries: 10000
  http:
    enabled: true
    listen: 127.0.0.1:80
  tls:
    enabled: true
    listen: 127.0.0.1:443
    trust_mode: auto
```

When DNS is enabled, Interceptor configures `127.0.0.1` as the container
resolver. It answers `A` queries for every declared simulated-service,
attacker-service, and identity hostname with `127.0.0.1`. It never contacts an
upstream resolver. Undeclared names return `NXDOMAIN` by default or `REFUSED`
when `unknown_policy: refused`; either result creates a coverage-gap event.
`max_queries` defaults to 10,000 and may be 1 through 1,000,000. Once exhausted,
queries fail `REFUSED` and only the first excess query is recorded, preventing
an application-controlled evidence flood. `listen` must use loopback port 53.

The HTTP front door routes exact HTTP `Host` values to the handler owning that
Blueprint hostname. The TLS front door requires TLS 1.2 or newer and also
requires SNI and HTTP `Host` to agree. Unknown or ambiguous origins fail with
HTTP 421 and a coverage gap. Internal service listeners remain loopback-only;
the front doors never proxy to an external destination. DNS and both front-door
listeners use `127.0.0.1`; this matches the resolver's declared-name answers and
avoids ambiguous IPv4/IPv6 binding.

TLS uses a private CA generated by `interceptor app build`. The certificate
covers only declared hostnames. Trust modes are:

| Mode | Behavior |
| --- | --- |
| `auto` | Adds the public CA to a recognized Linux PEM store when available and injects common PEM trust variables for OpenSSL/curl, Python Requests, Node, AWS SDKs, and gRPC. |
| `bundle` | Provides `/opt/interceptor/tls/ca.pem` and `INTERCEPTOR_CA_BUNDLE` without altering application-specific trust settings. |
| `application-managed` | Provides the same bundle but requires `application_managed_probe`, an absolute argv array run as the application user before startup. The command receives `INTERCEPTOR_TLS_PROBE_URL` and must return success only when the application's trust mechanism accepts the endpoint. |

The probe URL uses a reserved read-only `GET` endpoint that returns `204`; it
does not invoke or mutate the selected simulated service.

Example for a custom runtime:

```yaml
network:
  tls:
    enabled: true
    trust_mode: application-managed
    application_managed_probe:
      - /app/bin/verify-sandbox-tls
```

The generated public CA is not a credential. The leaf private key remains in
protected host application data and is mounted read-only for `sandboxd`; it is
not copied into the wrapper image or exposed to the application user. Standard
DNS is UDP/TCP rather than TLS. DNS-over-TLS and DNS-over-HTTPS are not included
in this release.

### Application interface

`interface` tells Interceptor how to invoke the application without requiring
the application to adopt an Interceptor-specific HTTP server. Exactly one
matching configuration block is required.

| `kind` | Configuration block | Application shape |
| --- | --- | --- |
| `http` | `http` | Existing local HTTP server. |
| `command` | `command` | Fresh CLI process for every call. |
| `stdio` | `stdio` | Long-running stdin/stdout process. |
| `repl` | `stdio` | Interactive process using a Linux pseudo-terminal by default. |
| `code` | `code` | Imported Python or Node module. |
| `external` | `external` | Executable implementing Interceptor's JSON-lines adapter protocol. |

Shared fields are:

| Field | Required | Meaning |
| --- | --- | --- |
| `kind` | yes | Selects the adapter. |
| `operations` | yes | Non-empty allowlist of callable operation IDs. IDs are lowercase identifiers. |
| `startup_timeout_ms` | yes | Positive readiness deadline, at most 600000. |
| `turn_timeout_ms` | yes | Positive deadline for one call, at most 600000. It also bounds model calls. |

HTTP operations additionally require `method` and a clean absolute `path`.
Other operations omit both. Command operations may contain `arguments`; each
entry has exactly one fixed `literal` or one JSON Pointer `from` the turn's JSON
input.

The adapter blocks are:

- `http`: required loopback `listen` URL and optional absolute `health_path`;
- `command`: `stdin` (`raw`, `json`, or `none`), optional
  `output_media_type`, `success_exit_codes`, and `concurrency` (maximum 32);
- `stdio`: `transport` (`pipes` or `pty`), `input_suffix`, optional
  `ready_pattern`, `response_mode` (`line`, `delimiter`, `prompt`, or `idle`),
  the matching `delimiter`, `terminal_pattern`, or `idle_ms`, optional
  `strip_echo`, `strip_terminal`, and `output_media_type`;
- `code`: `language` (`python` or `node`), import `module`, optional
  `initialize`, required `invoke`, `input_mode` (`single`, or Python-only
  `kwargs`), optional `init_arguments`, and `concurrency` (Python is limited to
  one; Node may use up to 32);
- `external`: exact `protocol: interceptor.application/v1`, optional inline
  `config`, optional sealed `config_asset`, and `concurrency`.

`config_asset` is part of the Sealed Environment and changing it invalidates
the seal. See [Application interfaces](application-interfaces.md) for complete
examples, call framing, response completion rules, and limitations.

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
  injection_namespaces:
    - id: inbox
      root: /state/app/inbox
      allow_create: true
      max_files: 100
      max_file_bytes: 1048576
```

`source` is a relative file or directory below the Blueprint directory.
`target` is a clean absolute container path. A writable seed target must fall
inside a declared mutable root. Read-only seeds are mounted directly from the
sealed assets; writable seeds are copied to a private folder for each session.
This prevents one test run from changing the starting files used by another.

`injection_namespaces` gives an external attack engine a stable logical ID for
a bounded part of a mutable root. Existing files can be changed through the
namespace. File creation requires `allow_create`, a real existing parent
directory, and positive `max_files` and `max_file_bytes` ceilings. Namespace
roots must not overlap. Capability manifests expose the ID and limits, not the
container path.

Assets may be regular files, directories, or symlinks that resolve within the
Blueprint. Hard links, escaping symlinks, special files, absolute sources, and
`..` escapes are rejected. Asset paths, modes, sizes, targets, and contents are
included in the seal.

## Synthetic service capabilities

Every service requires a unique lowercase `id`, a unique loopback `listen`
address in `host:port` form, and at least one unique hostname containing a dot.
The hostname resolves only inside the session container.

A stateful HTTP or MCP service can opt a collection into bounded injection-time
creation only when its sealed routes/tools already define a compatible create
operation:

```yaml
injections:
  create_collections:
    - collection: tickets
      max_resources: 1000
```

This permission does not place payloads in the Blueprint. It authorizes a
runtime payload to create a record using the service's existing create schema.

Any fixture, stateful, or MCP service can require a synthetic bearer identity:

```yaml
authorization:
  issuer: sandbox-identity
  audience: http://tickets.sandbox.local:18100
  required_scopes: [tickets:read]
  allow_delegated: true
```

`issuer` references `identity.issuers[].id`. `audience` is the value required in
the JWT `aud` claim. Every listed scope must be present in both the token and a
currently active grant or delegation. `allow_delegated` permits tokens that
identify a user as the subject and an agent as the actor. Authorization is
service-wide in the MVP; route- and tool-specific policies are not yet
supported.

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
      mode: static
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

## Synthetic identity

The optional `identity` section models workload identity, callers, and
delegated user access. It contains `issuers`, `principals`, `grants`,
`delegations`, `deliveries`, and an optional `inbound` configuration.

### Issuers

```yaml
identity:
  issuers:
    - id: sandbox-identity
      kind: oidc
      listen: 127.0.0.1:18300
      hostnames: [identity.sandbox.local]
      issuer_url: http://identity.sandbox.local:18300
      access_token_ttl_seconds: 900
```

The MVP supports `kind: oidc`, which provides OAuth discovery, JWKS,
client-credentials tokens, same-issuer token exchange, and optional AWS
container credentials. The URL must be an HTTP origin whose port matches
`listen` and whose hostname is loopback or one of `hostnames`. Token lifetime
defaults to 900 seconds and may be from 1 to 86,400 seconds.

### Principals, grants, and delegations

```yaml
identity:
  principals:
    - id: support-agent
      kind: agent
      issuer: sandbox-identity
      client_id: support-client
      client_secret: synthetic-client-secret
      claims: {name: Support Agent}
    - id: alice
      kind: user
      issuer: sandbox-identity
      claims: {name: Alice Example}
  grants:
    - id: agent-ticket-read
      principal: support-agent
      service: tickets
      scopes: [tickets:read]
  delegations:
    - id: alice-ticket-read
      agent: support-agent
      user: alice
      service: tickets
      scopes: [tickets:read]
```

Principal `kind` is `agent`, `user`, or `service`. Agent and service principals
may declare a unique OAuth `client_id` and synthetic `client_secret`; users may
not. Custom `claims` are copied into issued tokens, but reserved JWT and actor
claims cannot be overridden.

A grant authorizes the named principal directly. A delegation authorizes an
agent to act for one user. Delegated agents and users must belong to the same
issuer. Every target service must declare `authorization` for that issuer, and
every service and scope reference is checked while validating the Blueprint.

### Credential delivery

OAuth client environment delivery:

```yaml
deliveries:
  - id: support-oauth
    kind: oauth_client
    issuer: sandbox-identity
    principal: support-agent
    client_id_environment: TOOL_CLIENT_ID
    client_secret_environment: TOOL_CLIENT_SECRET
    token_url_environment: TOOL_TOKEN_URL
```

Projected workload token delivery:

```yaml
deliveries:
  - id: support-projected-token
    kind: projected_jwt
    issuer: sandbox-identity
    principal: support-agent
    audience: local-workload
    path: /run/interceptor-session/identity/support-agent.jwt
    environment_variable: AGENT_TOKEN_FILE
```

AWS SDK container-credential delivery:

```yaml
deliveries:
  - id: support-aws
    kind: aws_container_credentials
    issuer: sandbox-identity
    principal: support-agent
    endpoint_environment: AWS_CONTAINER_CREDENTIALS_FULL_URI
    authorization_token_environment: AWS_CONTAINER_AUTHORIZATION_TOKEN
```

Delivery-generated environment variable names must be unique and cannot
conflict with launch variables or static credentials. A projected path must be
beneath `/run/interceptor-session/identity/`. An AWS container-credential
delivery generates a loopback URI from the issuer's `listen` address so standard
SDK endpoint-safety checks accept it.

### Inbound caller identity

```yaml
inbound:
  issuer: sandbox-identity
  audience: support-agent
  application_header: Authorization
```

For an HTTP application, a harness can name a user principal with
`X-Interceptor-Caller` or `caller_principal` in a cooperative turn. Interceptor
replaces that harness value with a signed user token in the configured
application header. `audience` must name the agent principal that will receive
and exchange that caller token. The current automatic delivery supports only the standard
`Authorization` header and HTTP application interface. Other interfaces can
request a synthetic caller token explicitly and place it in their declared
input format.

See [Synthetic identity and delegated access](identity.md) for endpoint details,
token exchange, evidence, failure injections, snapshots, and limitations.

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
returns the configured status and either `json` or `body`. `mode` defaults to
`static`. A `dynamic` response must use a body-capable 2xx status and permits
runtime `service_response` injections after capture, making the endpoint a
bounded synthetic C2 channel. The response status, headers, methods, paths, and
destination remain sealed. `options.methods`
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
| `max_blob_bytes` | 104,857,600 | Per-blob and relevant bridge body bound; maximum 256 MiB. |
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
