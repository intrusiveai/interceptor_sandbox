# Overview

Interceptor Sandbox lets a security tester run an agentic application against a
small, reviewable synthetic world without giving the application ordinary
network access. The environment can include files, deterministic HTTP fixtures,
mutable JSON services, synthetic credentials, canaries, and attacker-controlled
capture endpoints. Model calls pass through a narrow host connector so real
provider credentials never enter the container.

The project is intended for repeatable security experiments such as:

- checking whether untrusted model or tool content changes application behavior;
- measuring whether synthetic secrets reach output, a model request, or an
  attacker sink;
- comparing transit-only simulation with mutations to persistent environment
  state;
- reproducing a result from a known logical state; and
- collecting verifiable evidence without a database or hosted control plane.

It is not an attack generator. The caller chooses test inputs, injection
payloads, sequencing, and interpretation. It is also not a containment boundary
for arbitrary hostile native code equivalent to a virtual machine, and it does
not claim byte-for-byte deterministic model output.

## How it works

The operator authors an Environment Blueprint and assets, validates them, and
seals them into a digest-bound definition. Interceptor then builds a small
application-specific wrapper image containing the Go supervisor and launches
one Docker container per session.

Inside the container, the supervisor starts:

- the onboarded application as a non-root process;
- declared fixture and stateful HTTP services;
- declared attacker sinks;
- an OpenAI-compatible model proxy; and
- private application and control bridge endpoints.

The container has no general network. A host-side connector forwards only the
supported model route to one operator-supplied provider origin and only for
explicitly allowed model names. The provider API key remains in host process
memory.

The host process exposes two unauthenticated loopback URLs while `run` or
`restore` remains attached:

- the **application URL** forwards only Blueprint-declared application routes;
- the **control URL** exposes cooperative turns, injections, snapshots, status,
  events, and attacker observations.

Operator-only information such as evaluation results and the state journal is
not exposed on the loopback control URL. CLI commands reach those endpoints
through the session's protected bridge.

## Design priorities

The MVP favors understandable local mechanisms:

- one Go binary and one container per session;
- Docker rather than Kubernetes;
- YAML and ordinary assets rather than a database;
- JSON metadata and append-only JSONL logs;
- content-addressed files for larger bodies and filesystem content; and
- logical replay-based restore rather than block, volume, or process cloning.

## Current capability boundary

The implemented application adapter is HTTP. The model seam supports
OpenAI-compatible `POST .../chat/completions`. Synthetic services are
`fixture_http` and `stateful_http`; attacker capture uses `http_sink`; virtual
time is fixed. Docker is the supported OCI runtime for the MVP.

Snapshots capture declared persistent state, not RAM, open connections,
threads, caches, or kernel state. An application must be restartable from its
declared mutable roots and managed service state before enabling snapshots.

## Typical lifecycle

1. Author and review an Environment Blueprint.
2. Validate and seal it.
3. Build and verify the wrapper image.
4. Start an attached session.
5. Submit application traffic or cooperative turns.
6. Arm authored injections and inspect observations.
7. Evaluate deterministic oracles and coverage gaps.
8. Create a snapshot at a quiescent point.
9. Restore one or more independent sessions from that checkpoint.
10. Stop the session, verify its logs, and export evidence.

