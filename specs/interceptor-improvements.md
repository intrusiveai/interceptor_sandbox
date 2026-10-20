# AEF Requirements for Interceptor Adaptive Simulation

## 1. Purpose and ownership

This document records the Interceptor capabilities that AEF and Operator depend
on and the AEF-side consumption rules.

The normative producer-neutral interface is owned by Interceptor Sandbox in
`specs/external-adaptive-simulation.md`. Interceptor remains independently
usable by PromptFoo and other harnesses and has no AEF dependency. This file
must not fork or redefine Interceptor's schemas.

## 2. Architectural relationship

```text
Interceptor CapabilityManifest -> AEF planning -> ScenarioBundle -> Operator

Operator adaptive attempts -> Interceptor execution

Operator CampaignExecutionBundle -----+
                                       +-> AEF CampaignAssessmentBundle
Interceptor EvidenceBundle(s) --------+
```

AEF is the primary semantic consumer of the cooperative capability and evidence
interfaces. Operator is the trusted execution, transport, and integrity broker:
it obtains the capability manifest before a run, executes producer-neutral
attempts, closes guest authority, retrieves an EvidenceBundle for every source
or restored session, and relays their unchanged verified digests in
`FinalSubmission`.

This design gives AEF full post-run truth without giving AEF live Interceptor
authority or exposing protected evidence to Operator's guest attack harness.

## 3. Integration capabilities

| ID | Capability | Components connected or benefited |
|---|---|---|
| INT-AEF-AD-001 | Producer-neutral attempt context and ancestry | Operator engine, Operator Runtime, Interceptor evidence, AEF correlation, third-party harnesses |
| INT-AEF-AD-002 | Feedback profiles and observation view | Operator engine, Interceptor policy, AEF assessment validity, third-party harnesses |
| INT-AEF-AD-003 | Machine-readable capability manifest | Interceptor, AEF planner, Operator compatibility checks, external planners |
| INT-AEF-AD-004 | Attempt-indexed evidence export | Interceptor finalizer, Operator final submission, AEF assessment |
| INT-AEF-AD-005 | Canonical payload artifact identity | Payload producer, Operator records, Interceptor delivery/evidence, AEF joins |
| INT-AEF-AD-006 | Managed HTTP response injection | Interceptor simulation fidelity, Operator scenario coverage, AEF reporting |
| INT-AEF-AD-007 | Native MCP result injection | Interceptor MCP fidelity, Operator scenario coverage, AEF reporting |
| INT-AEF-AD-009 | Typed event-match oracle | Interceptor evaluation and AEF deterministic assessment |
| INT-AEF-AD-010 | Operations harness interface | Operator guest, trusted Operator Runtime, Interceptor production boundary |
| INT-AEF-AD-011 | Native model-provider adapters | Interceptor target applications, Operator model relay parity, AEF compatibility reporting, third-party harnesses |
| INT-AEF-AD-012 | Host secret-store credential resolution | Interceptor host controller, model-provider adapters, model-provider authentication |
| INT-AEF-AD-013 | Closed DNS and locally trusted service TLS | Target application, simulated HTTP/MCP/identity/attacker services, Operator compatibility checks, AEF coverage analysis |

Use snapshot creation followed by trusted fresh-session restore for branching.

## 4. AEF consumption requirements

### 4.1 CapabilityManifest

AEF ingests the exact canonical Interceptor manifest by artifact digest. It uses
stable capabilities, operations, surfaces, identities, feedback profiles,
oracles, sinks, and limits for target binding and coverage planning.

Injection capabilities are target-specific. Each service identifies its role,
supported transports and surfaces, static/dynamic attacker response mode, and any explicitly
creatable state collections with their resource ceilings. Named filesystem
injection namespaces expose stable IDs and creation limits without exporting
container paths. A global surface list is only an index and never implies that
every surface applies to every target.

AEF MUST:

- reject unsupported schema or Interceptor release tuples;
- reject a manifest that exposes credentials, fixture bodies, canary values,
  raw host paths, or other forbidden fields;
- bind every ScenarioBundle to the exact manifest digest;
- treat a manifest change as a new planning input;
- never infer an undeclared capability from prose in a TargetAssessment.

### 4.2 Attempt identity and payload artifacts

AEF expects Operator's `CampaignExecutionBundle` and Interceptor's
`EvidenceBundle` to share producer-neutral campaign, session, attempt, parent,
and payload-digest identities.

`ActionSpec`, `MergeRecipe`, technique, and coverage-obligation references are
optional strategy provenance. AEF can use them when present but cannot require
Interceptor to interpret them or reject an independent producer for omitting
them.

The concrete payload originates with Operator's engine or another harness, not
AEF. Canonical artifact identity lets AEF prove which bytes Operator says it
sent and which bytes Interceptor says it applied or delivered.

### 4.3 Feedback separation

The ScenarioBundle requests one supported immutable feedback profile. Operator
and Interceptor independently enforce the effective profile. AEF planning may
describe useful protected evidence, but it must not cause that evidence to be
returned live unless the authorized profile expressly allows a normalized
oracle result.

AEF assessment preserves four source labels:

- target-visible;
- harness-visible;
- Operator-host-only;
- Interceptor-protected.

Protected post-run evidence can improve findings and future planning. It cannot
be represented as knowledge the live engine actually had.

### 4.4 EvidenceBundle set

AEF verifies that the EvidenceBundle set matches the FinalSubmission's project,
campaign, complete session/restore lineage, target, manifest, plan when
applicable, feedback profile, Interceptor release, attempt inventory, and
artifact digests.

It records completeness, truncation, missing attempts, contradictory lineage,
and unavailable artifacts explicitly. Missing evidence is not target
resistance, and a temporal event match is not causality without explicit
attempt or injection provenance.

The Operator host may relay each bundle, but it cannot edit one without
changing its digest and invalidating Interceptor authenticity.

### 4.5 Snapshot and restore correlation

ScenarioBundle submissions MAY request snapshot-creation and cumulative
committed-snapshot-byte ceilings through the public requested-limits fields.
Operator MUST resolve these requests against its configured ceilings; producer
identity MUST NOT grant execution authority. Operator controls when Interceptor
receives snapshot creation and restore requests, persists their usage in
one campaign ledger across target revisions while the same harness stays alive, and reports
requests, receipts, sizes, reasons, outcomes, and revision effects.

Interceptor independently enforces the session campaign association, host restore authority and
local hard limits and records authoritative snapshot identity, digest,
canonical size, parent lineage, environment/application/capability/release
bindings, and source/resulting session identities and revisions in the
corresponding EvidenceBundles. AEF correlates both views after the campaign.
Snapshot bytes remain Interceptor-managed and are not uploaded to AEF unless a
separate artifact-export policy explicitly requires them.

### 4.6 operations campaign access and reporting

The current MVP uses one integrated host service started by `interceptor run`
at `127.0.0.1:8080`, without integration authentication. Attach binds the campaign;
v1alpha2 retains supervisor-enforced campaign access, native limits and durable operations.
The same API provides snapshot restore, confirmed stop and protected evidence.
Restore replaces the target sequentially and returns a fresh session/revision;
exact retries cannot create a second replacement. Host-service failure is
terminal; no gateway restart protocol or remote provisioner is required.

Operator retains its own worker fencing, operation intents/results, cumulative
campaign accounting and per-revision native session bindings. Interceptor keeps
its journals, attribution and evidence. See [the local contract](../docs/local-api.md)
and native Section 13 for details and executable qualification.

## 5. Injection, provider and network behavior

- **INT-AEF-AD-006:** Managed successful HTTP responses can be
  rewritten at the service-ingress boundary with attempt and payload
  attribution.
- **INT-AEF-AD-007:** Native successful MCP tool results can be
  rewritten after execution and before application consumption, separately
  from model-message rewriting.
- **INT-AEF-AD-011:** Anthropic Messages, Gemini Developer API,
  Vertex Gemini, and Azure OpenAI Chat/Responses join OpenAI, Bedrock, and
  LiteLLM under the defined non-streaming text and client-function subset.
- **INT-AEF-AD-012:** The shared host resolver supports model
  provider credentials in AWS Secrets
  Manager, Azure Key Vault, Google Cloud Secret Manager, and HashiCorp Vault KV
  v2, while direct cloud workload identity remains available where appropriate.
- **INT-AEF-AD-013:** An optional bounded loopback DNS resolver
  authoritatively resolves declared service names without upstream forwarding.
  Shared exact-host HTTP and TLS 1.2+ front doors route to the managed handlers.
  Wrapper build generates a SAN-constrained sandbox certificate and supports
  `auto`, `bundle`, and probed `application-managed` trust without placing a CA
  signing key or server private key in the application image.

AD-012 is required for a conformant production controller deployment but not
for a local standalone development run.

## 5.1 Dynamic injection completeness

The Environment Blueprint defines a sealed capability envelope, never a
campaign payload inventory. A runtime engine supplies payload bytes, attempt
identity, selector, placement, scope, and timing through `POST /v1/injections`
or the typed operations `injection.arm` operation. Interceptor validates that the
specific target advertises the requested surface before it journals the arm.

The capability manifest reports injection surfaces per managed service,
identity service, and attacker service. It distinguishes static capture sinks
from dynamic attacker/C2 responses and exposes named filesystem namespaces
without their container paths. Invalid targets and response injection against a
static sink fail at arm time rather than remaining silently unmatched.

Dynamic attacker responses reuse `service_response`: the sink captures the
bounded request first, then Interceptor may rewrite only the successful sealed
default response body. Exact method/path, bounded query/non-sensitive-header
and body predicates, and per-definition journaled ordinals support reliable
adaptive sequencing. Runtime changes to listeners, destinations, route
allowlists, status, or security-sensitive headers remain prohibited.

Fidelity creation is bounded. A missing service resource requires a
Blueprint-opted collection, a positive resource ceiling, and validation by the
same sealed stateful HTTP route or MCP create-tool contract used by legitimate
traffic. A missing file requires a named injection namespace, an existing real
parent directory, explicit creation permission, and file-count and byte
ceilings. New targets accept only root replacement.

Local commands do not introduce a claimed syscall-level surface. A command
such as `curl` receives actual injected bytes from a declared managed or dynamic
attacker HTTP service. Shell output represented in a supported provider request
is separately eligible for `model_tool_result`. Arbitrary descendant-process
stdout, undeclared localhost sockets, and public-Internet requests are not
intercepted.

The operations operation set also includes `injection.delete`, bound to the same
campaign association, worker/revision attribution, expected native session revision, body digest, deadline and live request-handling rules
as other live mutations.

## 5.2 Closed network simulation

The capability manifest reports whether authoritative DNS and the shared
HTTP/HTTPS front doors are enabled, the DNS unknown-name policy and query
ceiling, and the TLS trust mode and minimum version. It omits listener
addresses, certificates, and private key material. This lets AEF and Operator
reject an incompatible ScenarioBundle without learning private topology.

DNS queries are protected evaluation evidence. Unknown names additionally
produce coverage gaps, allowing AEF to distinguish an attack resisted by the
application from an incomplete synthetic environment. The live feedback view
continues to apply its selected visibility profile.

## 6. Required integration slice

The minimum end-to-end product slice requires:

1. AD-003 for planning and compatibility;
2. AD-001 and AD-005 for attempt and content correlation;
3. AD-002 for bounded realistic adaptation;
4. AD-010 for secure Operator-to-Interceptor execution;
5. AD-004 for trusted post-run consolidation.

Production controller deployment additionally requires AD-012; its secret-store
configuration remains host-only and is neither an AEF planning input nor an
Interceptor target capability.

## 7. AEF-side conformance

AEF integration is conformant when:

- it plans only against capabilities from a verified exact manifest;
- a non-AEF producer can use the same Interceptor runtime schemas;
- no AEF-specific provenance is mandatory in an Interceptor attempt;
- AEF never supplies the concrete payload through the Interceptor contract;
- the live engine receives no protected evidence under a black-box profile;
- FinalSubmission binds Operator and Interceptor views without rewriting either;
- assessment conclusions identify the visibility and provenance of their
  supporting facts;
- a changed manifest, payload artifact, attempt graph, or EvidenceBundle digest
  causes deterministic rejection or explicit quarantine; and
- capability ingestion MUST preserve target operations, injection surfaces,
  feedback and limits without depending on upstream model-provider or
  authentication configuration;
- AEF never receives or requires Interceptor secret-store backend, locator,
  version, account, project, vault, authentication, or resolution metadata.

## 8. Non-goals

- Giving AEF a live Interceptor control credential.
- Making Interceptor call AEF directly.
- Copying proprietary AEF schemas into Interceptor.
- Treating AEF scenario prose as Interceptor authorization.
- Requiring Operator Sandbox for local or third-party Interceptor use.

## Live feedback

The native feedback contract provides default all-permitted selection within
immutable profiles, fixed exact-invocation receipts, 256 KiB content reads and
retained reads after restore/stop. Category availability, action delivery facts,
event-attributed oracle outcomes and truncation prevent empty/partial evidence
from being interpreted as negative results. See [the implemented contract](../docs/feedback.md).
