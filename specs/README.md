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

## Change discipline

Changes to externally visible behavior, file formats, schemas, APIs, lifecycle
semantics, or security guarantees should update the specification in the same
change as the implementation and tests. Machine-consumed schemas should live in
the repository-level `schemas/` directory once introduced and remain traceable
to the corresponding normative section here.
