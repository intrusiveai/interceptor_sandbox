# Specifications

This directory contains the product's normative specifications and supporting
architectural decisions.

## Authoritative specification

[`mvp.md`](mvp.md) is the authoritative specification for the local open-source
MVP. It defines the product contract, terminology, architecture, Environment
Blueprint format, lifecycle, APIs, recording, logical snapshots, restore
semantics, security model, and acceptance criteria.

Its status is **Draft 0.1**. Until a version is explicitly marked stable,
incompatible changes may be made through ordinary repository review.

## Architectural decisions

[`decisions/`](decisions/) contains rationale and migration records. Decision
documents explain why the specification takes its current form, but they are not
normative. If a decision document conflicts with `mvp.md`, `mvp.md` governs.

Current decisions:

- [`0001-local-runtime-redesign.md`](decisions/0001-local-runtime-redesign.md) —
  replace the distributed control-plane design with a local container runtime,
  file-backed state, and logical journal snapshots.
- [`0002-simulated-mcp-http.md`](decisions/0002-simulated-mcp-http.md) — add a
  protocol-aware, deterministic MCP tool service on the existing synthetic
  service, evidence, injection, and journal foundations.
- [`0003-host-model-provider-adapters.md`](decisions/0003-host-model-provider-adapters.md)
  — preserve native application model APIs while keeping all real provider
  access behind compatible host adapters.
- [`0004-modular-application-interfaces.md`](decisions/0004-modular-application-interfaces.md)
  — adapt HTTP, CLI, REPL, code-level, and custom application entry points to
  one operation contract inside the sandbox.
- [`0005-synthetic-identity-and-delegation.md`](decisions/0005-synthetic-identity-and-delegation.md)
  — simulate workload credentials and delegated users inside each session,
  without proxying a real identity provider or preserving bearer tokens in
  snapshots.
- [`0006-closed-dns-and-local-tls.md`](decisions/0006-closed-dns-and-local-tls.md)
  — add authoritative loopback DNS and shared HTTP/TLS hostname routing with a
  build-generated private sandbox CA.

## Adaptive simulation extension

[`external-adaptive-simulation.md`](external-adaptive-simulation.md) defines the
implemented producer-neutral interfaces for adaptive harnesses, capability
discovery, filtered feedback, attempt-indexed evidence, and a secure Cross-VM
Operator boundary. It is normative for those extension interfaces and augments
the local MVP contract. `interceptor-improvements.md` records the cross-project
priorities and AEF consumption rules without redefining these schemas.

## Change discipline

Changes to externally visible behavior, file formats, schemas, APIs, lifecycle
semantics, or security guarantees should update the specification in the same
change as the implementation and tests. Machine-consumed schemas should live in
the repository-level `schemas/` directory once introduced and remain traceable
to the corresponding normative section here.
