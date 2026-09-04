# Simulated MCP services

An `mcp_http` service lets an agent use MCP tools without contacting a real
remote MCP server. Interceptor runs the service inside the session container,
at a hostname declared in the Environment Blueprint. The application points
its MCP client at that local hostname instead of the real service.

This is useful when testing an agent that normally searches a remote knowledge
base, reads support tickets, or changes records through MCP. The test receives
repeatable synthetic results, cannot affect the real system, and retains useful
evidence about which tools the agent called.

## Blueprint declaration

```yaml
application:
  launch:
    environment:
      KNOWLEDGE_MCP_BASE_URL: http://knowledge-mcp.sandbox.local:18200/mcp

services:
  - id: knowledge-mcp
    kind: mcp_http
    listen: 127.0.0.1:18200
    hostnames: [knowledge-mcp.sandbox.local]
    endpoint: /mcp
    tools: assets/services/knowledge-mcp-tools.yaml
    seed: assets/services/knowledge-mcp-state.json
```

`endpoint` is the single MCP HTTP path. `tools` contains the tool definitions
and their simulated behavior. `seed` is required when any tool has a
`stateful` behavior; omit it when all tools use fixed fixtures. All referenced
files become part of the Sealed Environment.

Configure the agent's MCP client explicitly with the local URL. Interceptor
does not redirect a hard-coded remote URL and does not copy real MCP
credentials into the container.

## Optional OAuth protection

An MCP service can require a synthetic identity:

```yaml
services:
  - id: knowledge-mcp
    kind: mcp_http
    listen: 127.0.0.1:18200
    hostnames: [knowledge-mcp.sandbox.local]
    endpoint: /mcp
    tools: assets/services/knowledge-mcp-tools.yaml
    authorization:
      issuer: sandbox-identity
      audience: http://knowledge-mcp.sandbox.local:18200
      required_scopes: [knowledge:use]
      allow_delegated: true
```

The protected service publishes OAuth Protected Resource Metadata and returns a
`WWW-Authenticate` discovery hint when authentication is missing. It accepts
RS256 bearer tokens issued by the declared local identity issuer and checks the
audience, required scopes, and current grant or delegation before MCP sees the
request.

This supports configured client-credentials and token-exchange clients. It does
not yet implement interactive browser authorization, PKCE, dynamic client
registration, or different scopes for individual tools. See [Synthetic
identity and delegated access](identity.md).

## Tool definition file

```yaml
tools:
  - name: search_documents
    title: Search documents
    description: Search the synthetic company knowledge base
    input_schema:
      type: object
      properties:
        query: {type: string}
      required: [query]
    output_schema:
      type: object
      properties:
        answer: {type: string}
    behavior:
      kind: fixture
      fixtures:
        - id: retention-policy
          priority: 100
          match: {query: retention}
          result:
            content:
              - type: text
                text: Records are retained for seven years.
            structured_content:
              answer: Records are retained for seven years.

  - name: get_ticket
    description: Read one synthetic support ticket
    input_schema:
      type: object
      properties:
        id: {type: string}
      required: [id]
      additionalProperties: false
    behavior:
      kind: stateful
      action: get
      collection: tickets
      key_argument: id

  - name: patch_ticket
    description: Change the status of a synthetic support ticket
    input_schema:
      type: object
      properties:
        id: {type: string}
        status: {type: string}
      required: [id, status]
      additionalProperties: false
    behavior:
      kind: stateful
      action: patch
      collection: tickets
      key_argument: id
      patch_fields: [status]
```

Names must contain 1–128 letters, digits, dots, underscores, or hyphens and
must be unique within the service. A description and an object-shaped
`input_schema` are required. `title` and `output_schema` are optional.

Interceptor publishes the schemas in `tools/list`. It checks required
top-level arguments, their basic JSON types, and `additionalProperties: false`.
It does not implement complete JSON Schema validation or verify that a result
conforms to `output_schema`; the schema still helps the MCP client and model
construct appropriate calls.

## Fixture behavior

Fixture tools return reviewed, unchanging results. `match` is a subset of the
tool's argument object, so the request may contain additional arguments unless
the input schema rejects them. Higher-priority fixtures are considered first.
Two fixtures that could match the same arguments at the same priority make the
Blueprint invalid.

The result supports MCP text content and optional `structured_content`.
`is_error: true` can model a tool-reported failure. When no fixture matches,
Interceptor returns an MCP tool error and records a coverage gap instead of
inventing a plausible result. An encoded fixture result or individual MCP seed
record may not exceed 1 MiB; combined runtime results are checked against the
same limit.

Fixture results are sealed and immutable. Use stateful behavior when a test
needs mutations, fidelity injections, or snapshot-restored tool data.

## Stateful behavior

Stateful tools operate on the same canonical collection/key record model used
by `stateful_http`. The seed is a JSON object whose first level contains
collections and whose second level contains record keys:

```json
{
  "tickets": {
    "ticket-001": {"id":"ticket-001","status":"open"}
  }
}
```

Supported actions are:

| Action | Configuration and behavior |
| --- | --- |
| `list` | Requires `collection`; returns all records in key order. |
| `get` | Requires `collection` and `key_argument`; returns the matching record. |
| `create` | Requires `collection` and `key_field`; stores the argument object under that field's value. |
| `replace` | Requires `collection` and `key_argument`; replaces the complete record with the argument object. |
| `patch` | Requires `collection`, `key_argument`, and `patch_fields`; changes only those fields. |
| `delete` | Requires `collection` and `key_argument`; removes the matching record. |

Missing and conflicting records return an MCP tool result with `isError: true`.
Every successful mutation is written and flushed to the state journal before
the MCP response is acknowledged.

## Evidence, injections, and snapshots

Interceptor records MCP initialization, tool discovery, tool calls, tool
errors, and coverage gaps in the protected evaluation event history. Tool-call
evidence includes the service, tool name, arguments, selected behavior, and
returned synthetic result. It does not record transport authorization headers.

The existing injection surfaces apply without an MCP-specific injection API:

- `environment_state` can change an existing record owned by a stateful MCP
  tool. It can create a missing record only when the service explicitly opts
  that collection into injection creation and a sealed MCP create tool validates
  the complete new object. A later `get`, `list`, or other stateful action
  encounters that changed record through the real MCP data path.
- `mcp_tool_result` can rewrite a successful native result after the real MCP
  tool executes and before the MCP client receives it.
- `model_tool_result` can still change the tool-result content immediately
  before an agent sends it to the model. This works after the framework
  converts its MCP result into native Chat, Responses, or Converse conversation
  history.

For an MCP state target, use the MCP service ID, seed collection, and record
key:

```json
{
  "service": "knowledge-mcp",
  "collection": "tickets",
  "key": "ticket-001"
}
```

`service_state_match` oracles use the same address. Evidence that contains an
injection payload can therefore be connected to the existing injection and
oracle evaluation flow.

Stateful MCP mutations and fidelity injections use ordinary `service_put` and
`service_delete` journal operations. A snapshot records the journal position;
restore starts from the sealed MCP seed and reapplies operations through that
position. Fixture definitions do not need journaling because they are already
part of the Sealed Environment. In-flight MCP requests and protocol connection
state are not snapshotted.

## Implemented MCP subset

The MVP supports:

- one HTTP endpoint with individual JSON-RPC 2.0 requests over `POST`;
- the `initialize`, `ping`, `tools/list`, and `tools/call` methods;
- the `notifications/initialized` notification;
- initialization negotiation for protocol versions `2024-11-05`,
  `2025-03-26`, `2025-06-18`, and `2025-11-25`;
- stateless direct `tools/list` and `tools/call` requests;
- complete JSON responses with MCP text and structured tool results; and
- request bodies up to 1 MiB.

The MVP does not support:

- SSE or incremental streaming, HTTP `GET`, resumable streams, or MCP session
  IDs;
- batch JSON-RPC requests, tool-list pagination, or server-initiated messages;
- resources, prompts, completions, sampling, roots, elicitation, subscriptions,
  or cancellation of an in-progress call;
- image, audio, embedded-resource, or resource-link result content;
- interactive OAuth authorization, PKCE, dynamic client registration, or
  non-bearer MCP transport authentication;
- dynamic response templates or arbitrary code in fixture definitions;
- spawning an MCP implementation from the Blueprint; or
- forwarding requests to an actual remote MCP server.

An unsupported method fails explicitly and creates a coverage gap. If one of
these capabilities is necessary to the legitimate application workflow, use a
reviewed local MCP server bundled in the application image or extend the
`mcp_http` service before treating the environment as complete.

When `network.tls` is enabled, the same MCP handler is available through the
shared HTTPS hostname front door with the generated sandbox certificate. This
adds MCP-over-HTTPS transport without changing the supported MCP method or
content subset.
