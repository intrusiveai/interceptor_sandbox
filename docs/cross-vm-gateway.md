# Native operation wire contract

The current service starts with `interceptor run` on `127.0.0.1:8080`.
See [Local Operator integration](local-api.md) for attachment, campaign binding,
restore, stop and evidence. This file retains its old name for existing links.
There is no separate gateway process or integration authentication in the MVP.
The service uses the native v1alpha2 codecs and supervisor-owned execution store
specified below. Its internal principal identifies the local instance and worker identifies the caller;
they do not authenticate an external caller.

## Request and response contract

The public JSON object has exactly `request` and `body` fields. The request is:

```json
{
  "api_version": "interceptor.dev/operation-request/v1alpha2",
  "request_id": "request-01",
  "operation_id": "bind-01",
  "operation": "session.owner",
  "session_id": "sess-approved",
  "campaign_id": "campaign-01",
  "worker_instance_id": "worker-01",
  "run_revision": 1,
  "expected_session_revision": 1,
  "body_digest": "sha256:<digest of exact embedded body bytes>",
  "deadline": "2026-09-15T21:00:00Z"
}
```

`body_digest` hashes the exact UTF-8 JSON bytes of `body`, including interior
whitespace. The host proxy preserves those bytes. Unknown fields, duplicate
keys, multiple JSON values, non-object bodies and nesting deeper than 64 are
rejected. Requests are bounded by the larger of the session artifact envelope limit and 5 MiB. IDs are
bounded correlation values, not credentials. `run_revision` is a positive integer.
The Go codec and committed fixture in `internal/adaptive/testdata` define the
wire representation; unrelated native resource versions retain their own codecs.

The response object contains `status` (native HTTP status), `body` (native JSON)
and `session_revision`. Public HTTP status follows `status`, except native 204
uses HTTP 200 to preserve the envelope and revision. An in-progress duplicate
returns 202 and `body.code: operation_in_progress`. Framing/authentication failures
use the gateway error envelope; a transport failure is 503 with
`session_unavailable_outcome_unknown`, not proof that an effect failed to start.

## Campaign association and attribution

Before admitting the guest, `/v1/attach` internally sends `session.owner` with
the body below. Public binding calls are rejected; use the returned session and the caller's own worker/revision attribution in native requests.

```json
{
  "action": "bind",
  "session_id": "sess-approved",
  "campaign_id": "campaign-01",
  "worker_instance_id": "worker-01",
  "allow_target_stop": false
}
```

Body session/campaign must match the envelope. The optional legacy body worker
field is ignored for access; the envelope supplies caller attribution. Initial
association checks native revision and restored attempt campaign. Campaign and
stop policy remain fixed; initial worker, revision and service principal are
historical metadata. Same-campaign callers need no worker ownership transfer.
A planned restore creates a fresh session associated with the same campaign.
`session.owner` remains host-only and never becomes a harness tool.

Campaign ID is the access boundary. Worker ID and campaign run revision are
required attribution on native operation requests, not permission requirements;
a different worker or revision of the same campaign may interact with the current
target and read retained campaign sessions. The internal service principal is
provenance, not an exclusive owner. Historical `session.owner`/`owner` names remain
wire labels for campaign association, stop policy and terminal closure.
They do not establish worker ownership or require transfers after restore.

Injection cleanup accepts any existing injection in the addressed campaign session,
including checkpoint-restored injections with no arm record in the new execution
store. Missing injections return not-found. Duplicate injection IDs cannot overwrite
existing definitions; use a fresh ID or delete before reusing an ID. Mutated payloads
use new operation IDs. Native expected-session-revision checks, current-target
addressing, closure, visibility, budgets and stop opt-in remain in force.

Duplicate-effect comparison excludes worker/revision attribution, including the
legacy worker field in a `session.owner` body. The full original request and its
fingerprint remain immutable audit data; a retry returns the original result and
never overwrites the originating caller. Other request fields/command bytes remain
fixed for an exact retry, including target, operation ID, request ID, deadline and
expected native revision. A new worker can also query `operation.status` without
copying the original request. Lifecycle duplicate comparison likewise excludes
caller attribution while preserving the admitted request in its ledger.


Close admission using the same session/campaign identity and `action: close_execution`,
omitting `allow_target_stop`. Closure is permanent, survives request retries,
and does not wait for a running operation. It bypasses the expected-revision check
so concurrent completion cannot prevent shutdown. Queued effects recheck closure
and revision at dispatch; already-admitted effects may finish. Close remains
available when operation-record capacity is full. Binding cannot reopen closure.

After binding, direct local control mutations are rejected. The explicit local
administrator `interceptor stop` command remains available and closes admission.
Operator integrations use `/v1/lifecycle` `session.stop`, which enforces the recorded
`allow_target_stop` choice; the administrator command is not an integration fallback.

## Operations

| Operation | Body and additional binding |
| --- | --- |
| `session.owner` | Bind/close body above. |
| `session.status` | `{}`; returns `session` metadata, `owner`, and `execution_available`. |
| `capabilities.read` | `{}`; native secret-free CapabilityManifest. |
| `artifact.register` | `descriptor` and base64 JSON `content`; native artifact limits apply. |
| `artifact.read` | `digest`; returns digest and base64 JSON `content`. |
| `attempt.register` | Native AttemptContext; matching envelope attempt ID, digest and campaign. |
| `injection.arm` | Native `definition`, optional `eligible_turn`; matching registered attempt ID/digest. Injection ID must not already exist. |
| `injection.delete` | Existing `injection_id` in this campaign session, including restored state. |
| `application.invoke` | Native TurnRequest; matching registered attempt ID/digest and payload. |
| `observation.read` | `turn_id` for exact invocation (optional only when unambiguous), or `receipt_id` for a fixed view; matching envelope attempt ID/digest. |
| `observation.content.read` | `receipt_id`, `entry_id`, byte `offset`, `max_bytes` (1–262,144); bounded bytes from the fixed permitted view. |
| `operation.status` | Exactly one of `receipt_id` (native application turn) or `operation_id` (durable mutation record). |
| `snapshot.create` | Optional `label` and `description` (UTF-8, at most 4,096 bytes); supervisor records campaign ID from the request/session and supplies remaining snapshot-byte allowance. Response includes that hash-covered metadata. |
| `session.stop` | `{}`; requires the bind-time target-stop opt-in and closes admission. |

Mutations other than closure require `expected_session_revision` to match at
dispatch. A different campaign or closed execution cannot register artifacts or attempts, arm
injections, invoke the application or create snapshots. Read-only reporting,
campaign-scoped injection deletion and explicitly permitted stop remain available
under the same campaign. Deletion does not undo prior fidelity/application
effects. Do not restart stopped targets for cleanup.

Native attempt/artifact registry ceilings, turn/time limits and consumed snapshot
counts/bytes are session-owned. Mutation dispatch is serialized in the session supervisor.
Operator additionally retains cumulative campaign accounting across restored sessions.
The native snapshot-byte ceiling is `min(max_blob_bytes * max_snapshots, 16 TiB)`;
committed usage is never refunded on deletion. Host retries do not reset native budgets.

## Request loss and uncertain outcomes

The integrated local service owns the API and provider bridge. Service loss is
terminal; no gateway replacement or process takeover is supported. The default
spool still reserves closure/status capacity alongside the operation lane, and
the supervisor serializes native effects. See [Local integration](local-api.md)
for the separate durable restore/stop ledger and exact lifecycle retry behavior.

Mutations retain their immutable request fields, fingerprint, admission/completion
times and response in `execution.json`. The fingerprint covers the canonical native
request (which contains the exact body digest). The same operation/request identity
with changed command fields conflicts. Concurrent duplicates cannot produce another
effect. A completed exact duplicate returns its saved response even after its
original deadline; an expired new command never dispatches. Reads use fresh IDs and
are not stored as mutation records.

Use a fresh `operation.status` request with `operation_id` to reconcile a lost
reply. States are `running`, `completed` and `unknown`; `running` includes queued
work. A missing record or unavailable lookup never proves non-execution. Do not
resubmit an uncertain mutation as a new operation. Existing `receipt_id` lookup
continues to address native application turns while the supervisor is available.

Records have a 4,096-entry admission ceiling, 4 MiB per response and a 64 MiB
state-file ceiling. Pending operations reserve response capacity before admission.
Closure has reserved capacity. No records are evicted to make room for new effects.
This initial implementation atomically rewrites the bounded state file; it favors
simple durable recovery over high-volume throughput. Capacity exhaustion rejects
new work before dispatch.

State is fsynced and its digest appended to the existing hash-chained state journal
before dispatch and before reporting completion. Missing, corrupt or mismatched
state fails closed. A supervisor restart closes execution and marks unfinished
records unknown; it does not resume an owner. The read-only retained path reports
available status/operation records without starting the target, and marks unfinished
records unknown when the live service is unavailable. Native receipt service may
then be unavailable. Persistence failure or an over-limit/uncertain effect result
closes admission; report the outcome honestly.

## Evidence and migration

Existing events, native state transactions, registries and protected export remain.
Events identify dispatched operations and returned native status. The evidence tar
also includes `execution.json`, verified against `state.jsonl` by both log
verification and export. Owner attribution and unresolved operation outcomes are
reflected in the existing EvidenceBundle campaign/completeness fields. This is protected
host evidence, not live harness context. A partial session may retain unavailable
or unknown outcomes rather than a fabricated success.

This is a breaking replacement for the experimental v1alpha1 gateway. Create a
new native session under the updated runtime; existing sessions without
`operation_api_version` cannot be attached through v1alpha2. There is no in-place
lease migration or endpoint alias. Historical resource/evidence formats remain
readable. Capability output now advertises `session.owner`, changing its digest;
recompute and qualify pinned capabilities. The existing `cross_vm_operations`
CapabilityManifest field retains its spelling for resource-codec compatibility.

Local codec, journal, concurrency and socket integration tests are included.
They do not establish production container/host qualification, provider behavior,
or a released immutable source pin.

Campaign checkpoint discovery uses the integrated host routes
`POST /v1/snapshots/list` and `/v1/snapshots/inspect`, not new native operations.
See [metadata, pagination and earlier-session scope](local-api.md#snapshot-inventory-and-metadata).

## Fixed live feedback update

See [live feedback](feedback.md) for observation selection, immutable native
receipt/entry reads, profile boundaries, availability/completeness and retained
source-session reads. Rebuild the host and supervisor together; capabilities
advertise the new view version and bounded content-read operation. Operator
and harness runtime support remains a separate implementation deliverable.
