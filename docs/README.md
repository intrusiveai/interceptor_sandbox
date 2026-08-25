# Interceptor Sandbox documentation

Interceptor Sandbox is a local-first security test harness for deliberately
onboarded agentic applications. This documentation describes the implemented
MVP. The files in [`specs/`](../specs/) remain the normative product and design
specification.

Start here:

1. [Overview](overview.md) — what the sandbox is, its vocabulary, and its limits.
2. [Getting started](getting-started.md) — build and run the reference system.
3. [Application integration](application-integration.md) — requirements for
   onboarding another application.
4. [Environment Blueprint reference](environment-blueprint.md) — the complete
   YAML schema and the synthetic capabilities available to an environment.

Reference and operational guides:

- [Architecture](architecture.md)
- [CLI reference](cli-reference.md)
- [Local API reference](api-reference.md)
- [Injections and evaluation](injections-and-evaluation.md)
- [Snapshots and restore](snapshots-and-restore.md)
- [Evidence and storage](evidence-and-storage.md)
- [Security and trust model](security.md)
- [Troubleshooting](troubleshooting.md)

## Vocabulary

| Term | Meaning |
| --- | --- |
| Environment Blueprint | Authored `environment.yaml` plus its referenced assets. |
| Sealed Environment | A validated Blueprint bound to immutable content digests by `environment.lock.json`. |
| Environment Instance | The live synthetic world materialized inside one session. |
| Session | One isolated container, application process, state journal, event log, and lifecycle. |
| Snapshot | The CLI term for creating a logical restore point. |
| Checkpoint | The durable manifest that identifies a snapshot position. |

