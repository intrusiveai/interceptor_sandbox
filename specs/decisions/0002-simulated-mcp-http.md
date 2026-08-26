# Decision 0002: Simulated MCP over HTTP

- Status: accepted
- Date: 2026-08-25

## Context

Agent frameworks such as Strands can connect to remote MCP servers over HTTP.
Interceptor sessions intentionally have no general network access, so the real
remote endpoint is unavailable. The existing synthetic hostnames and loopback
listeners can route the same application request to `sandboxd`, but the generic
HTTP fixture and CRUD services do not implement MCP's JSON-RPC envelopes,
dynamic request IDs, tool discovery, or tool-result conventions.

Bundling a separate MCP server in every application image would provide wire
compatibility, but it would make evidence, coverage gaps, fidelity injections,
and snapshot state application-specific. Forwarding to the real remote MCP
server would reintroduce real credentials, non-repeatable state, and external
side effects that cannot be restored from an Interceptor snapshot.

## Decision

Add `mcp_http` as an in-process synthetic service kind. The Blueprint declares
one local HTTP endpoint and one sealed YAML tool definition asset. Tools use
either deterministic fixture behavior or stateful collection/key behavior
seeded by sealed JSON.

The first implementation supports individual, non-streaming JSON-RPC requests
for initialization, ping, tool discovery, and tool calls. It fails closed on
unknown protocol behavior. It does not attempt to be a general MCP proxy or a
complete MCP server.

Stateful MCP tools implement the same managed-service interface and journal
operations as `stateful_http`. Therefore:

- mutations are durable before acknowledgement;
- `environment_state` can target existing MCP-owned records;
- `service_state_match` can evaluate those records;
- MCP activity and gaps enter the protected evaluation event history; and
- restore rebuilds MCP state from the sealed seed plus the journal.

The existing `model_tool_result` surface remains responsible for transit
simulation after an agent framework represents the MCP result as a model tool
message. Direct interception between the MCP client and synthetic server is not
added in this decision.

## Consequences

Agents must accept an explicitly configured MCP endpoint and use the declared
synthetic HTTP hostname. Hard-coded remote endpoints are unsupported.

The service gives common tool workflows deterministic, snapshot-aware behavior
without adding another process, database, or credential path. Protocol
features such as SSE, sessions, server-initiated requests, resources, prompts,
OAuth, TLS, and non-text content remain explicit future work rather than
partially implemented behavior.

A project that requires capabilities outside this subset may bundle a reviewed
local MCP server in its application image. Interceptor cannot claim managed
state, coverage completeness, or restore semantics for that separate server
unless the application supplies the necessary persistence and evidence hooks.
