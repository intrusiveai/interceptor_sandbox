# Overview

Interceptor Sandbox provides agentic AI application security testers the
ability to perform comprehensive attack scenarios in a simulated environment
where the agent can access a real LLM. All other services are simulated,
making the agent believe that it is operating in a normal environment (the
agent can make tool calls, get responses and change state of the underlying
environment) while giving the tester the ability to control tool responses.
Having control of tool responses gives the tester the ability to thoroughly
and easily test direct and indirect prompt injection scenarios across every
potential attack surface.

Additionally, Interceptor Sandbox captures every request/response and
state change that occurs in the sandbox environment, saves the changes as
snapshots that can be restored, and evaluates the interactions with
deterministic oracles that provide indisputable evidence of an attack's
success/failure, which is useful to compare against an external LLM judge that
may be analyzing the attack success rate separately.

More specifically, Interceptor Sandbox lets a security tester run an agentic
application against a small, reviewable simulated world without giving the
application ordinary network access. The world can include files, services with
predefined responses, services whose records can change, fake credentials,
synthetic agent and user identities, recognizable test secrets called canaries,
and endpoints that capture attempted data theft. Model calls pass through a
narrow connection on the host so real provider credentials never enter the
container.

The project is intended for repeatable security experiments such as:

- checking whether untrusted model or tool content changes application behavior;
- checking whether an agent preserves a delegated user's identity and stays
  within its synthetic permissions;
- measuring whether synthetic secrets reach output, a model request, or an
  attacker sink;
- comparing content changed only on its way to the model with content changed in
  the environment's stored data;
- reproducing a result from a known logical state; and
- collecting verifiable evidence without a database or hosted control plane.

It is not an attack generator. The tester chooses the inputs, injected content,
order of operations, and interpretation. It also does not isolate hostile native
code as strongly as a separate virtual machine, and it cannot promise that a
model will generate exactly the same response every time.

## How it works

The operator writes an Environment Blueprint and its supporting files, validates
them, and seals them. Sealing records digital fingerprints so later changes can
be detected. Interceptor then packages its supervisor with the application and
launches one Docker container for each session.

Inside the container, the supervisor starts:

- the onboarded application as a non-root process;
- simulated services with predefined or changeable data;
- a synthetic identity issuer that can grant, exchange, and verify short-lived
  local credentials;
- endpoints that capture attempted data theft;
- an optional authoritative DNS resolver and exact-host HTTP/HTTPS front doors;
- a model proxy that understands the supported native application APIs; and
- private application and control bridge endpoints.

The container has no general network. A host-side connector accepts only the
supported model routes and explicitly allowed model names. OpenAI Chat and
Responses, Azure OpenAI, Anthropic Messages, Gemini/Vertex `generateContent`,
Amazon Bedrock Converse, and LiteLLM remain in their native shapes. Provider
credentials remain on the host and production secrets can be resolved from a
supported secret store or provider workload identity.

`interceptor run` owns the container and API at `127.0.0.1:8080`. No separate
gateway or integration credential is needed. The first attach binds one campaign;
native v1alpha2 requests perform experiments and create snapshots. The same
service restores into a fresh session, stops the target, and downloads protected
evidence. It stays available after target stop; Ctrl+C ends the host service.
One active target is supported. See [Local integration](local-api.md).

## Design priorities

The MVP favors understandable local mechanisms:

- one Go binary and one container per session;
- Docker rather than Kubernetes;
- YAML and ordinary assets rather than a database;
- readable JSON metadata and logs that add new entries without rewriting old
  ones;
- shared storage that avoids duplicating identical large content; and
- restore by reapplying recorded state changes rather than copying an entire
  disk or running process.

## Current capability boundary

Test harnesses connect to Interceptor over a local HTTP URL, while the
application itself may use HTTP, a CLI, stdin/stdout, a REPL, Python/Node calls,
or a small external adapter. The application's model connection may use a
supported non-streaming OpenAI, Azure OpenAI, Anthropic Messages,
Gemini/Vertex, Bedrock Converse, or LiteLLM route; the selected host adapter
must support the same native protocol.
Simulated services are
`fixture_http`, `stateful_http`, and the non-streaming MCP subset provided by
`mcp_http`; those services can optionally enforce synthetic agent or delegated
user permissions. `http_sink` supports multiple static capture destinations and
explicitly enabled dynamic response endpoints for bounded C2 simulation.
Declared hostnames may resolve through a recorded loopback DNS service with no
upstream forwarding, and the same HTTP/MCP handlers may be exposed through a
TLS 1.2+ hostname router using a build-generated sandbox CA.
Virtual time is fixed.
Docker is the supported container runtime for the MVP.

Snapshots capture declared saved state, not information that exists only in
memory, running tasks, open connections, or caches. Before enabling snapshots,
an application must be able to restart using its declared saved folders and
simulated service data.

## Typical lifecycle

1. Author and review an Environment Blueprint.
2. Validate and seal it.
3. Build and verify the wrapper image.
4. Start an attached session.
5. Submit application traffic or cooperative turns.
6. Enable test injections and inspect what the application did.
7. Evaluate predefined success checks and identify missing simulated behavior.
8. Create a snapshot after the application reaches a safe save point.
9. Restore one or more independent sessions from that checkpoint.
10. Stop the session, verify its logs, and export evidence.
