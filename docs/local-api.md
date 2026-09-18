# Local Operator integration (MVP)

This is the current deployment and lifecycle contract for Operator and
Interceptor. It supersedes the separate authenticated gateway and remote
runner/provisioner design. Native experiment requests retain
`interceptor.dev/operation-request/v1alpha2`; lifecycle messages use
`interceptor.dev/local-lifecycle/v1alpha1`.

## Startup and scope

`interceptor run --environment <sealed-directory> [provider options]` starts
one target container and serves HTTP at **127.0.0.1:8080 in that same host
process**. There is no separate gateway command, configurable public listener,
TLS termination, integration token, or Operator-to-Interceptor authentication.
Operator and Interceptor run on the same trusted host. All local host users
with loopback access are inside this MVP trust boundary; campaign IDs are
correlation, not credentials. Model-provider credentials remain host-only and
continue to use their existing provider configuration.

The listener is reserved before target startup. A second run fails without
creating a target; live (including paused) leftover `interceptor-sess-*` containers
also prevent startup until an administrator resolves them. Stopped historical
containers and evidence remain untouched. The service does not adopt orphaned
containers or resume an old campaign. `run` no longer opens separate public
application/control convenience ports. Private Unix/file-spool bridges and the
target's `--network none` containment remain unchanged.

The process retains its original environment, image, transport and provider
configuration for all restores. It exposes no shell, arbitrary host path, image
selection, command execution, URL forwarding or remote provisioning operation.
The harness cannot access this API: its tools still go through Operator's policy
broker over the existing non-network guest channel.

## Routes and response framing

All routes require POST and strict JSON objects. Unknown fields, duplicate keys,
trailing values and excessive nesting are rejected. JSON responses use the native
wrapper `{ "status": 200, "body": { ... }, "session_revision": 0 }`.
Native status 204 is carried in an HTTP 200 wrapper. Successful evidence download
is the one binary response. Send `Content-Type: application/json`.

| Route | Request | Purpose |
|---|---|---|
| `/v1/attach` | Lifecycle version, `campaign_id`, `worker_instance_id`, optional `allow_target_stop` | Bind the first campaign and return current session, native binding and capabilities. |
| `/v1/operations` | Existing v1alpha2 `{request, body}` envelope | Native experiments, snapshot creation, status, observations and admission closure. |
| `/v1/lifecycle` | Lifecycle request described below | Restore or confirmed target stop. |
| `/v1/snapshots/list` | Lifecycle version, `campaign_id`, optional `source_session_id`, `offset`, `limit` | List verified checkpoint metadata across retained campaign sessions. |
| `/v1/snapshots/inspect` | Lifecycle version, `campaign_id`, `source_session_id`, `checkpoint_id` | Inspect one retained checkpoint without contacting/restarting the target. |
| `/v1/status` | Lifecycle version, `campaign_id`, optional `operation_id` | Current instance/session lineage or one lifecycle operation record. |
| `/v1/evidence` | Lifecycle version, `campaign_id`, `session_id` | Download protected evidence for one stopped session belonging to this instance. |

No `/v1/cross-vm`, lease endpoint, separate gateway listener or standalone
`interceptor restore` launcher is supported. Existing administrator CLI commands
remain host administration tools; Operator uses this API.

## Attach and native experiments

```json
{
  "api_version": "interceptor.dev/local-lifecycle/v1alpha1",
  "campaign_id": "campaign-01",
  "worker_instance_id": "operator-worker-01",
  "allow_target_stop": true
}
```

The first attach persistently binds the instance to this campaign. Another
campaign is rejected for the entire process lifetime, including after stop.
Repeating attach with the same stop policy returns the current binding while
execution is open. After closure, use `/v1/status` and `/v1/evidence`; attach does
not reopen execution. Changing
that policy is a conflict. `allow_target_stop` defaults to false; Operator maps
its explicit `--stop-interceptor` choice to this value.

The successful body contains `campaign_id`, `binding`, native `session` metadata
and `capabilities`. `binding` contains `session_id`, `worker_instance_id`, and
`run_revision` (initially 1). The session and revision describe the target lifecycle;
the worker describes the caller of this attach/start response, not an owner.
Attach requires the actual Operator `worker_instance_id`; same-campaign reattach
may use a different worker. The Interceptor process identity is separately exposed
as `instance_id` in status. Native requests carry the requesting Operator worker and
campaign revision, without comparison to initial attachment. Operator computes the
exact body/attempt digests, supplies unique request/operation IDs and bounded
deadlines, and uses current native `session.revision` for mutations. Native state
revision and caller campaign revision are distinct.

The existing [native operation wire guide](cross-vm-gateway.md) defines bodies,
canonicalization, expected revisions, durable operation records and response
limits. Public `session.owner` accepts only `close_execution`: attachment owns
binding. Public `session.stop` uses `/v1/lifecycle` so success means that the host
has confirmed removal. IDs beginning `local-` are reserved for service-generated
native operations. Native reads for retained sessions preserve their original
campaign association; new mutations must address the active session. Stale mutation requests
are rejected before forwarding, even if the old container still has records.
During an admitted restore, a terminal close may still use its source binding
until the replacement result is durably published; it closes the whole campaign.

Explicit `close_execution` is terminal for the campaign in this service. It
bypasses the mutation queue, closes future restore admission, and does not itself
stop a target whose shutdown was not opted into. Already-dispatched work may
finish and remains reportable. Campaign-scoped injection deletion remains available
for bounded cleanup. For a planned healthy restore, pause/drain work and use the
restore operation; do not issue terminal campaign closure first.

Native execution closure is also terminal for the campaign. After each forwarded
mutation, the integrated API reconciles authoritative native execution state;
uncertain mutation responses or transport failures close admission conservatively.
It persists the instance ledger's `closed` flag before releasing the mutation gate
or returning the result. If that persistence fails, in-memory closure and the
ledger-failure latch still block mutations and restore. Known rejections do not
close an otherwise healthy campaign. Status reads, campaign-scoped injection cleanup
and opted-in final stop remain available subject to existing failure checks.

Before a restore closes or stops the source, the integrated handler independently
queries native `session.status` under the mutation gate. It requires the matching
session/campaign association, open execution and running target; a cached `closed: false`
or a running container alone is insufficient. Closed, unavailable or malformed
native state rejects restore and closes campaign admission without replacing the
target. The subsequent native close acknowledgement must confirm planned
controller closure, not an earlier fault closure. Rejection never starts a new
session or advances the run revision. These checks live in the existing Interceptor
process; they introduce no separate lifecycle service or recovery mechanism.

## Campaign access and attribution

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

## Snapshot and restore

Create a checkpoint with native `snapshot.create`, body
`{ "label": "baseline", "description": "Before trying the next mutation" }`.
Both fields are optional. The authoritative `campaign_id` comes from the native
request envelope and associated session; a campaign override in the body is
rejected. Description is valid UTF-8, at most 4,096 bytes, and stored verbatim.
Creation returns the checkpoint including campaign ID and description. Both are
covered by its integrity hash. Changing the description changes the command:
use a new operation ID rather than mutating an existing retry.
Snapshots contain supported logical target state, not a process-memory image or
harness conversation. To restore:

```json
{
  "api_version": "interceptor.dev/local-lifecycle/v1alpha1",
  "campaign_id": "campaign-01",
  "worker_instance_id": "operator-worker-01",
  "run_revision": 1,
  "session_id": "CURRENT_SESSION_ID",
  "operation_id": "restore-0001",
  "operation": "snapshot.restore",
  "checkpoint_id": "CHECKPOINT_ID"
}
```

Lifecycle requests require `worker_instance_id` and positive `run_revision` for
caller attribution. They need not match the session-start attribution and never
set the server-maintained lifecycle revision, which increments once per restore.

An optional `source_session_id` selects a checkpoint from a retained session of
this same instance/campaign; it defaults to `session_id`. The outer session ID
must always identify the current active target. No arbitrary inventory, data
root or cross-campaign checkpoint is accepted.

At a healthy, quiescent boundary the service validates the checkpoint, journals
lifecycle admission, blocks competing mutations, closes the old native execution,
and **stops/removes the old container before starting its replacement**. The
replacement has a fresh native session ID, preserved restore ancestry and a new
campaign association and recorded startup attribution. A successful response includes `binding`,
ready native `session`, source ID and checkpoint ID. The API address and campaign
binding stay unchanged. An explicit restore authorizes replacement of the source
container even when optional final target shutdown is disabled.

Operator persists this successful binding and advances its campaign run revision
once, then returns the transition result to the same running harness. Its
conversation, hypotheses, snapshot notes, scratch and pipe assignments survive;
immutable launch manifests remain unchanged. The harness updates its active
revision before submitting more work, and Operator routes it to the new target.
Native run revision matches that sequence. Operator keeps campaign counters
cumulative: checkpoint-native counters are not the cumulative campaign budget.
A known preflight rejection leaves the original healthy target, harness and
revision usable; failure after closure/stop or an uncertain outcome is terminal.
A concurrent terminal close prevents a ready replacement from continuing.

## Snapshot inventory and metadata

List all checkpoint records retained by the attached campaign, including source
sessions replaced by earlier restores:

```json
{
  "api_version": "interceptor.dev/local-lifecycle/v1alpha1",
  "campaign_id": "campaign-01",
  "offset": 0,
  "limit": 100
}
```

Send this to `POST /v1/snapshots/list`. Optional `source_session_id` filters to one
retained campaign session. The response body contains `campaign_id`,
`checkpoints` (an array, including when empty), `total` and `next_offset` when
another page exists. Each record includes checkpoint `id`, `campaign_id`, optional
`description` and `label`, `source_session_id`, optional `parent_checkpoint`,
`created_at`, `status`, `canonical_size_bytes`, compatibility/digest fields and
`hash`. Creation, list and inspect return the same hash-covered metadata.

Inspect one record with `POST /v1/snapshots/inspect`:

```json
{
  "api_version": "interceptor.dev/local-lifecycle/v1alpha1",
  "campaign_id": "campaign-01",
  "source_session_id": "SOURCE_SESSION_ID",
  "checkpoint_id": "CHECKPOINT_ID"
}
```

The body of a successful response is the checkpoint. Use its source-session and
checkpoint IDs together for restore, including when the source is an earlier
session. Description is purpose text, not an instruction or an access credential.
Restore selects existing metadata; it does not replace campaign ID or description.
These read-only routes use the lifecycle version but are not lifecycle mutations,
do not need an operation ID or worker/revision attribution, and do not increment
any revision or reopen execution. They remain available after closure/stop while
this host process is alive. They never restart a target or return snapshot bytes.

The default page size is 100; maximum size is 1,000. Offset is nonnegative and at
most 10,000. At most 10,000 records may be read in one selected inventory; narrow
by source session if needed. Encoded responses are capped at 4 MiB. The list is
ordered by creation time, source session, then checkpoint ID. Source membership
is captured per request; pagination is not frozen across concurrent creation or
restore. Refresh from offset zero if the inventory changes.

Invalid request fields return 400; another campaign or a source outside the
attached campaign returns 403; missing inspected checkpoint returns 404; inventory
or response limit returns 413 (reduce page size or filter by source); unreadable,
corrupt or inconsistent metadata returns 503. No silent partial list is returned.
Read errors do not close healthy execution. A verified `ready` record does not
promise that restore will pass full environment/journal/blob preflight.

For compatibility, older checkpoints and unassociated administrative snapshots
may omit `campaign_id`/`description`. The source session's verified campaign
association scopes reads of these records; the API does not rewrite stored
metadata or its hash. Operator can project the attached campaign and empty
description to its harness while retaining the original native receipt.
An explicitly different checkpoint campaign is rejected on inspect/list/restore.

## Stop and evidence

Use the same lifecycle envelope with `operation: "session.stop"`, a fresh
`operation_id`, and no checkpoint/source fields. The stored attach policy must
allow shutdown. The host closes native execution, waits for target shutdown,
confirms container removal, and returns `phase: "stopped"`. Stop/cleanup failure
returns an explicit unconfirmed error; it never starts another target.

The host service remains available after target stop for status and evidence.
`POST /v1/evidence` can export both source and restored session IDs. Only stopped
or error sessions whose containers are absent qualify. It generates a host-owned
temporary archive, verifies the existing native journals/blob/checkpoint inputs,
and streams at most 256 MiB with `X-Content-SHA256: sha256:...`. No caller chooses
a host output path. Native evidence completeness/partial markers are retained.
Archive generation uses native storage/resource limits; the download ceiling is
not a universal disk quota. Exports are generated one at a time.

Operator verifies the bounded archive and native hashes, attaches each native
session to its campaign revision, and never exposes protected evidence to the
harness. Without final-stop permission, reporting may remain
`pending-target-stop`; an administrator can stop the target, after which evidence
collection can finish. Ctrl+C stops the host service and any remaining target.
After service exit, retained evidence remains available through administrator
`logs verify` / `logs export`, not through a resurrected campaign.

## Lost responses, concurrency and limits

Lifecycle admission and result are atomically saved/fsynced in
`<data-root>/instances/<instance-id>.json`. Each instance retains at most 1,024
lifecycle records within a 64 MiB ledger bound with reserved response capacity; it rejects new records before effects when full. A lifecycle
operation ID is bound to its decoded command fields, excluding worker/revision
attribution. Repeating the same command
returns the original result (including the replacement session ID), or 202 while
running. Changed fields conflict. Duplicate lookup occurs **before** stale-session
validation, so retrying a completed restore with its original source session
cannot create another container. `/v1/status` can query the record by ID.

Accepted lifecycle effects continue after an HTTP client disconnect, with a
five-minute operation deadline. Retrying an uncertain operation with a new ID is
forbidden. Native experiment records remain in each supervisor's journal-backed
v1alpha2 store; use native `operation.status` for those. Lifecycle records and
native operation records are separate, explicitly named namespaces.

The process is the owner of the provider bridge, API and target lifecycle.
There is no gateway replacement protocol or automatic process restart in this
MVP. Host-service loss is terminal; a saved `running` record has an unknown outcome
and is audit data, not an instruction to resume. Operator stops its harness and
reports uncertainty. Later administrator cleanup/export may finish reporting.
Remote deployment, multiple instances, auth, supervision and restart recovery are
deferred requirements, not missing local-MVP transport components.

## Compatibility

Use rebuilt Interceptor supervisor images and the matching host executable for
this campaign-access policy. The MVP retains its existing wire version names and
historical `owner` field/operation names, but earlier exclusive-worker implementations
are not behaviorally compatible. Attach now requires actual Operator worker
attribution, and lifecycle restore/stop requires worker plus campaign revision.
Update clients with those fields. Rebuild the host binary and supervisor image
together for snapshot description/campaign metadata support. New host list/inspect
routes read retained checkpoint files; no separate gateway deployment is required. Existing historical request fingerprints remain
readable audit data; there is no live-session adoption or in-place upgrade.

## Executable qualification

`go test ./internal/localapi` covers campaign mismatch, exact restore replay,
stale sessions, shutdown policy, failed stop, and close during replacement startup.
Closure regressions also cover native 5xx and oversized mutation results, transport
failure, stale API closure state, unavailable native status, native closure during
restore preflight, ledger-write failure, known rejection, and retained cleanup.
For a fresh reference environment, the standalone test client is:

```sh
go run ./examples/local-api-smoke --output /tmp/local-api-evidence \
  --data-root /absolute/path/to/the/test-data-root
```

It creates its own test campaign, invokes the reference application before and
after restore, verifies filesystem rollback when `--data-root` is supplied,
rejects a stale mutation, stops the target and verifies complete protected exports
for both sessions. It is a qualification client, not Operator implementation.

See [the recorded Docker validation](local-api-validation.md) for tested source
bytes, runtime scope and evidence results.

## Fixed live feedback update

See [live feedback](feedback.md) for observation selection, immutable native
receipt/entry reads, profile boundaries, availability/completeness and retained
source-session reads. Rebuild the host and supervisor together; capabilities
advertise the new view version and bounded content-read operation. Operator
and harness runtime support remains a separate implementation deliverable.

## macOS host lifetime

`interceptor run` automatically manages `caffeinate -i -w <host-pid>` during active
execution, restore and cleanup. It releases the assertion while serving only retained
evidence. Screen lock is supported; actual sleep, logout or Docker interruption ends
execution. See [host lifetime and recovery](host-lifetime.md).
