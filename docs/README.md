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
4. [Application interfaces](application-interfaces.md) — HTTP, CLI, REPL,
   direct code, and custom adapter configuration.
5. [Environment Blueprint reference](environment-blueprint.md) — the complete
   YAML schema and the synthetic capabilities available to an environment.

Reference and operational guides:

- [Architecture](architecture.md)
- [Model providers](model-providers.md)
- [Adaptive simulation interface](adaptive-simulation.md)
- [Delivery contracts and public capability context](delivery-contracts.md)
- [Native operation wire contract](cross-vm-gateway.md)
- [Host credential stores](credential-stores.md)
- [Synthetic identity and delegated access](identity.md)
- [CLI reference](cli-reference.md)
- [Local API reference](api-reference.md)
- [Injections and evaluation](injections-and-evaluation.md)
- [Simulated MCP services](mcp-services.md)
- [Snapshots and restore](snapshots-and-restore.md)
- [Evidence and storage](evidence-and-storage.md)
- [Spool file management and evidence purge](file-management.md)
- [Docker identity and recovery](docker-identity.md)
- [Security and trust model](security.md)
- [Troubleshooting](troubleshooting.md)

## Vocabulary

| Term | Meaning |
| --- | --- |
| Environment Blueprint | Authored `environment.yaml` plus its referenced assets. |
| Sealed Environment | A validated Blueprint that has been locked to exact file and image versions so later changes can be detected. |
| Environment Instance | The live synthetic world materialized inside one session. |
| Session | One isolated run of the application and its synthetic environment. |
| Snapshot | The CLI term for creating a logical restore point. |
| Checkpoint | The small record that identifies exactly which saved state belongs to a snapshot. |
| State journal | An ordered history of environment changes used to rebuild saved state. |
| Digest | A digital fingerprint used to detect whether an image or file changed. |
| AttemptContext | Immutable producer-neutral identity and ancestry for one payload attempt. |
| CapabilityManifest | Deterministic, secret-free description of an environment's executable surface. |
| ObservationView | Feedback-filtered live result visible to an attack harness. |
| EvidenceBundle | Post-run authoritative attempt, event, snapshot, and artifact index. |
| Campaign association | Session/campaign access, worker/revision audit attribution, permanent admission closure and a recorded target-stop choice. |
| Loopback | A network address available only on the local machine or inside the container, rather than on the wider network. |

Reference pages retain exact field names and implementation terms because users
need them when configuring or diagnosing the sandbox. Each guide introduces
those terms in plain language before relying on them.

- [Local Operator integration and lifecycle API](local-api.md) — current same-host MVP contract.
