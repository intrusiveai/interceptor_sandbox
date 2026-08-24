# Interceptor Sandbox Reorientation

## Recommendation

Replace the distributed platform design with a local, single-session runtime:

- one target-specific sandbox container per session;
- one compiled Go binary used both as the host CLI and the in-container
  supervisor;
- YAML Environment Blueprints and small JSON files for metadata;
- append-only JSONL for observations and restorable state changes;
- local content-addressed files for large values;
- no Kubernetes, database, object service, tenant model, or authentication;
- no block-, volume-, or filesystem-level cloning.

The terminology used by the redesigned product is:

- **Environment Blueprint** — the authored YAML and referenced assets;
- **Sealed Environment** — the validated, content-hashed immutable artifact; and
- **Environment Instance** — one mutable runtime materialization owned by a
  session.

A snapshot should be a logical restore point. It identifies a Sealed
Environment, an exact application image, and a committed position in a state
journal. Restore starts a fresh container, materializes a new Environment
Instance from the same Sealed Environment, and applies the journal through that
position. This is analogous to rebuilding a database from a base backup plus a
write-ahead log, while remaining honest about one important constraint: process
memory is not captured. A snapshot-capable application must externalize its
durable state to declared paths or sandbox-managed services.

This is the right point to pivot. The implementation has built substantial
control-plane foundations, but it has not yet built the inference proxy,
Environment Instance runtime, injection machinery, oracles, attacker sinks, or
snapshotting. Most of
the code that embodies the old deployment assumptions can be retired without
throwing away a working sandbox runtime, because that runtime does not yet
exist.

## Terminology migration

| Legacy term or identifier | Replacement |
|---|---|
| world bible | Environment Blueprint |
| sealed world | Sealed Environment |
| per-session world/overlay | Environment Instance |
| `world.yaml` | `environment.yaml` |
| `world.lock.json` | `environment.lock.json` |
| `world_digest` | `environment_digest` |
| `world_state` injection surface | `environment_state` |
| `world_sealed` provenance | `environment_sealed` |
| `interceptor world ...` | `interceptor environment ...` |

Legacy `World` identifiers in the current Go implementation should be renamed
when their packages are retained. Compatibility aliases are unnecessary before
the MVP has published a stable external format.

## What the current design and code contain

The specification currently describes a hosted multi-tenant platform with:

- a dedicated EKS account, VPC, cluster, node pools, custom AMIs, gVisor, CRDs,
  namespaces, CSI volumes, and EBS snapshots;
- PostgreSQL, Garage, an OCI registry, Redis, ClickHouse, Vault/KMS, and an event
  bus;
- independently deployed gateway, controller, registry, build, evaluation,
  recording, model, source-staging, webhook, and policy services;
- Cognito scopes, GitHub App/OIDC source staging, quotas, warm pools, and public
  ingress;
- a generalized entropy ledger and paired statistical replay design.

The implementation reflects that order of work. It currently provides:

- legacy `World` types plus sessions, budgets, channels, provenance, and taints;
- bearer-token authorization and tenant-scoped HTTP endpoints;
- memory and PostgreSQL repositories plus migrations;
- filesystem and Garage content-addressed blob stores;
- a `SandboxSession` CRD, Kubernetes runtime adapter, controller, manifests, and
  isolation-baseline tests;
- an in-memory hash-chain recorder.

It does not yet provide the main product loop: running an application in an
Environment Instance, proxying its model calls, serving its dependencies,
applying injections, evaluating outcomes, or restoring state. The existing work is
therefore best treated as a prototype of domain rules and integrity checks, not
as the architecture to preserve.

## What must remain

The following are product requirements rather than infrastructure choices.
Removing them would change the problem being solved.

| Requirement | Why it remains |
|---|---|
| Deliberate onboarding | The sandbox runs a known application whose launch, state, credentials, and dependencies have been reviewed. It is not a malware detonation system. |
| An Environment Blueprint | The synthetic organization, identities, files, fixtures, mutable services, canaries, and time must be coherent and reviewable. |
| Build-time generation only | An LLM may help author the Blueprint, but the runtime must not invent responses. Unknown requests fail closed and become coverage gaps. |
| Exact Environment/application binding | A session and snapshot must identify the Sealed Environment and application image digests that produced them. |
| One Environment Instance per session | Session mutation and model context must never leak into another run. A local container gives this a direct implementation. |
| No real credentials or unmediated egress | Prompt injection can induce consequential actions. Live credentials and general outbound network access must not enter the container. |
| Mandatory inference proxy | Delivery can only be measured at the point where content actually enters model context. Model interactions must be recorded. |
| External attack strategy | The sandbox hosts, injects, observes, and scores. It does not generate or adapt attacks. |
| Attack/control/evaluation separation | A harness must not accidentally receive oracle results or delivery ground truth that a real attacker would not know. This remains useful even without auth. |
| Provenance and append-only recording | Reviewers need to distinguish application behavior, Sealed Environment behavior, harness injection, and attacker-sink behavior. |
| Three-valued delivery results | `DELIVERED_SUCCEEDED`, `DELIVERED_RESISTED`, and `NOT_DELIVERED` must remain distinct. |
| Deterministic oracles and canaries | Objective evidence such as sink hits, state changes, and canary exposure is more trustworthy than an LLM judge. |
| Stateful dependency semantics when required | A mutate-then-read workflow cannot be faked with independent static responses. Unsupported dependencies must fail onboarding rather than behave incoherently. |
| Quiescence and explicit restartability | A logical snapshot is only valid when declared persistent state is stable and sufficient to restart the application. |

## What should be simplified

| Current concept | MVP replacement |
|---|---|
| Hosted control plane | A single `interceptor` Go binary and an in-container `sandboxd` mode. |
| Hosted environment registry | Blueprint directories addressed by path and a generated `environment.lock.json` digest. |
| PostgreSQL control state | Atomic JSON metadata files under a local data directory. |
| Garage/object storage | A local SHA-256 blob directory used by journals and recordings. |
| Kubernetes operator and CRD | `interceptor run` creates and supervises one Docker/Podman container. |
| Namespace-per-session | Container-per-session plus a unique local session directory. |
| EBS CoW overlay and CSI snapshot | A logical change journal and content-addressed file delta records. |
| Internet-facing authenticated API | Loopback-only host endpoints and protected Unix sockets, with no auth. |
| Separate microservices | In-process modules in `sandboxd`: inference proxy, fixture router, state stores, injection controller, sinks, recorder, and oracle evaluator. |
| Event bus and SSE fan-out | Direct JSONL append plus optional per-process SSE from memory. |
| Tier A/B platform fleet | Static HTTP fixtures plus a small declarative stateful HTTP store. Exact service emulators are plugins or unsupported. |
| Full syscall/FUSE interception | Protocol-level observation first; file-state deltas at snapshots; optional Linux tracing later. |
| Entropy ledger and paired mode | State restoration only. Record model traffic, but make no exact-future-replay or paired-statistics claim in the MVP. |
| Environment OCI images | A reviewed Blueprint directory plus a local lock file; the application remains an OCI image. |
| Complex onboarding service | CLI validation, optional profiling imports, human review, and deterministic sealing. |

## What should be dropped from the MVP

- Kubernetes, CRDs, controllers, RBAC, NetworkPolicy, EKS, node pools, AMIs,
  CSI, EBS, infrastructure-as-code, and cluster conformance.
- PostgreSQL, migrations, Garage, Redis, ClickHouse, Vault/KMS, event buses,
  registries for Sealed Environment artifacts, and distributed leases.
- Tenancy, quotas across users, Cognito, OAuth scopes, API keys, public ingress,
  WAF configuration, and webhook delivery.
- GitHub repository registration, OIDC staging, GitHub App token management,
  policy bundle compilation, and source quarantine.
- Warm pools, horizontal scaling, schedulers, and multi-node capacity planning.
- Full process checkpointing, block snapshots, CRIU, snapshot-capable
  filesystems, and copy-on-write volume requirements.
- Generalized deterministic entropy virtualization, exact replay of stochastic
  futures, paired experiments, null-pair calibration, and built-in statistical
  comparison.
- Tier C service engines, transparent arbitrary-protocol interception, and a
  promise to emulate every cloud or SaaS API.
- An LLM judge, automated attack generation, C2 tasking, downstream pivoting,
  and general-purpose DAST behavior.
- PCAP, OpenTelemetry, OCSF/ECS, and analytics exports until native JSONL has
  proven insufficient.

These are not permanently forbidden features. They are excluded because none is
needed to validate the local product loop and each creates a second platform to
operate before the first sandbox is useful.

## Logical snapshot design

The replacement snapshot mechanism has two append-only records:

1. `events.jsonl` is the observation history. It records what happened and is
   hash-chained for tamper evidence.
2. `state.jsonl` is the authoritative restore journal. It records only
   deterministic operations needed to reconstruct sandbox-managed state.

Sandbox-managed service mutations are journaled before they are acknowledged.
Control mutations such as injection arming and virtual-clock changes are also
journaled immediately. The application filesystem is handled without FUSE: at
snapshot time, the app is brought to quiescence, declared mutable roots are
compared with the previous committed file manifest, new content is written to
the local blob directory by digest, and one atomic transaction containing
`file_upsert` and `file_delete` operations is appended.

A checkpoint file then records:

- Sealed Environment and application digests;
- state-journal sequence and head hash;
- the resulting mutable-file manifest digest;
- injection, clock, oracle, and budget state at that sequence;
- the parent checkpoint, if any;
- runtime schema version and creation time.

Restore never rewinds or edits a running journal. It creates a new session from
the immutable checkpoint:

1. verify the checkpoint, journal chain, environment digest, app digest, and
   blobs;
2. create a clean runtime directory and materialize a new Environment Instance
   from the Sealed Environment baseline;
3. replay journal transactions through the checkpoint sequence;
4. launch a fresh container and let the application load its externalized
   state;
5. append a new control event identifying the parent checkpoint.

This model provides cheap branches naturally: restoring the same checkpoint
twice creates two independent Environment Instances that share immutable Sealed
Environment assets and content-addressed blobs but have separate future
journals.

It deliberately does not claim to restore:

- process memory, goroutine/thread stacks, open connections, or kernel state;
- uncommitted writes or background work that has not reached quiescence;
- state outside declared mutable roots or sandbox-managed services;
- the exact future output of a stochastic model.

An application that depends on such state is runnable but not snapshot-capable,
unless it is adapted to externalize that state.

## Local containment model

The default safe profile should use Docker or Podman with:

- `--network none`;
- read-only container root, explicit writable state mounts, and tmpfs scratch;
- a non-root application user, all capabilities dropped, no-new-privileges,
  PID/CPU/memory/disk limits, and no container socket;
- no provider or real-service credentials in the container;
- canary credentials accepted only by local synthetic services;
- host/container communication through mounted Unix sockets;
- host listeners bound only to `127.0.0.1` or `::1`.

The host CLI bridges selected application and control routes between loopback
TCP and container Unix sockets. A host-side model connector holds provider
credentials. The in-container inference proxy talks to it over a protected Unix
socket, while the application can reach only the inference proxy on container
loopback.

This is a meaningful containment boundary for a cooperatively onboarded target,
but it is not equivalent to a dedicated gVisor/Kata cluster. The MVP must state
that it is not for unknown binaries or malware and should prefer rootless
containers or an optional gVisor runtime where available.

## Disposition of the current implementation

| Current area | Disposition |
|---|---|
| `internal/domain` | Reuse concepts and selected validation, rename legacy `World` contracts to the new Environment vocabulary, and remove tenant, hosted-auth, distributed lifecycle, and paired-entropy fields. Add Sealed Environment lock, local session, checkpoint, and journal-operation types. |
| `internal/recording` | Reuse the hash-chain design. Replace the in-memory-only chain with an append-only JSONL writer and verifier. |
| `internal/blobstore/filesystem.go` | Reuse the traversal-safe, digest-verifying local blob logic after renaming it as the only MVP blob store. |
| `internal/httpapi` | Keep strict JSON decoding and response hygiene patterns; replace authenticated multi-tenant routes with loopback/Unix-socket session routes. |
| `internal/controlplane` | Replace with a local session supervisor and lifecycle state machine. |
| `internal/auth` | Remove. Local host trust and socket/loopback exposure are the MVP boundary. |
| `internal/store/postgres.go`, `migrations` | Remove. Use atomic JSON metadata and append-only JSONL. |
| `internal/blobstore/garage.go` | Remove. |
| `api/v1alpha1`, `internal/controller`, `internal/sessionruntime` | Remove. Container lifecycle is managed directly by the CLI. |
| `deploy/` and Kubernetes-focused docs | Remove or archive outside the active implementation. Replace with a Dockerfile, target-wrapper build flow, and local examples. |
| Current tests | Retain tests for fail-closed behavior, channel separation, provenance, hash integrity, digest verification, and idempotent local operations. Delete cluster-, auth-, tenant-, quota-, and PostgreSQL-specific tests. |

Because the referenced implementation repository contains unrelated uncommitted
documentation edits, the architectural pivot should be applied in a new branch
or a new clean repository rather than by destructively rewriting that working
tree.

## MVP completion test

The reoriented MVP is complete when a developer can:

1. author and validate an Environment Blueprint directory;
2. seal it as a Sealed Environment and obtain a stable digest;
3. wrap a known application image with the sandbox runtime;
4. start one no-egress local container;
5. drive the application from an external harness;
6. observe all model calls and deterministic synthetic-service interactions;
7. arm a harness-supplied injection and distinguish delivery from outcome;
8. observe an objective canary or attacker-sink result without leaking the
   oracle verdict into the attack response;
9. create a logical snapshot;
10. restore it into a fresh Environment Instance and verify the declared state
    matches;
11. inspect and verify the local JSONL trajectory and state journal; and
12. stop and remove the container without losing its evidence directory.

The standalone specification in [`../mvp.md`](../mvp.md) defines this design
without relying on the previous specification or implementation.
