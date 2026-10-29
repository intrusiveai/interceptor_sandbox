# Live feedback and bounded content reads

Implemented contract, 2026-09-16. Native request envelopes remain
`interceptor.dev/operation-request/v1alpha2`. Observation views now use
`interceptor.dev/observation-view/v1alpha2`; matching host/supervisor builds and
clients are required. The integrated same-host localhost API and network-disabled
target remain unchanged. There is no new process or harness network endpoint.

## Profiles and selection

A session keeps one immutable feedback profile:

| Profile | Available feedback |
|---|---|
| black-box | Target response bytes and basic operation status. |
| diagnostic | Black-box plus normalized operation errors and recorded injection-delivery facts. |
| oracle-assisted | Diagnostic plus supported, attributed event-backed oracle outcomes. |

Raw errors, event logs, private state, fixture/canary values and oracle definitions
are not returned. Operator may narrow feedback further; it cannot broaden the
native profile. Profile changes require a separate properly configured session.

Native AttemptContext now accepts optional `observation_selection`:

```json
{"mode":"all-permitted"}
```

Omission has the same default behavior and preserves historical attempt digests.
A narrower selection is `{"mode":"selected","kinds":["target_output",
"injection_delivery"]}`. Allowed kinds are target_output, operation_error,
injection_delivery and oracle_outcome. Unknown/duplicate kinds, empty selected
lists and kinds attached to all-permitted are invalid. Selection is covered by
the attempt digest; it does not mutate an existing attempt or profile. A request
for a profile-disallowed kind reports withheld, without hidden detector details.
Basic operation status always remains present.

### Operator feedback narrowing

`AttemptContext.feedback_profile` MUST equal the immutable native session profile,
not Operator's effective harness profile. For a diagnostic session and a black-box
bundle, register `feedback_profile: diagnostic` and select only `target_output`.
The native view still reports diagnostic. Operator projects that view to black-box
and suppresses all other feedback, including legacy aliases and detailed errors.
Narrowing does not require a new session or a native profile change.

Operator intersects native permissions, the bundle profile, trusted host policy
and per-attempt selection. Send a nonempty result as an explicit native selected
list. If the intersection is empty, omit selection at registration and do not
collect a native observation view or content for that attempt. This omission is
only a valid registration default; Operator must not subsequently interpret it as
permission to collect all feedback. It returns its own receipt with no entries
and explicit withheld/not-requested categories. No new native selection mode is
introduced.

The [Operator mapping](../../operator_sandbox/schemas/FEEDBACK_CONTRACT.md#native-profile-and-effective-harness-policy)
is normative for this adapter. Operator recomputes guest category states under
its effective policy rather than forwarding native states. Native receipt access
still validates the source session/profile; Operator separately checks its filtered
entry map and effective policy on every harness read, including after restore.

## Collect a fixed observation view

After `application.invoke` completes, send native `observation.read` with the
registered attempt ID/digest in the envelope and this body:

```json
{"turn_id":"turn-1"}
```

Use the invocation's actual returned turn ID. `{}` is accepted only when the
attempt has exactly one invocation; multiple invocations return 409
`observation_turn_required` rather than choosing the newest one. A missing/running
invocation returns 409 `observation_not_ready`. Neither rejection closes execution.
The private GET `/v1/observations/{attempt-id}` supports equivalent `turn_id` or
`receipt_id` query parameters for administrative use.

The returned ObservationView preserves operation state, target output descriptor
and normalized observations, and adds:

- `receipt_id`, campaign/session/attempt/context-digest/turn identities;
- basic operation state, response code and exit code when supplied by the adapter;
- `captured_at`, `window_start`, `window_end`, `through_event_seq`;
- `collection_state` (`complete` or `partial`);
- one category summary for each of the four kinds, with `state` and optional reason;
- up to 64 immutable entries with ID, kind, visibility, source, assurance,
  availability, optional artifact, truncation and original size;
- `hash`, covering the serialized view with the hash field omitted.

Category states are available, empty, withheld, not_requested, unavailable or
partial. Empty means no recorded observation in this scope, not a proven absence
of effects. A legitimate empty target response is an available zero-byte artifact.
Storage failure or missing evidence is explicitly unavailable/partial. Artifact
size/digest describe the stored permitted bytes; original_size_bytes describes
captured input to this projection, not bytes never observed by the adapter.
Failed invocations with no output are unavailable, not empty success. Returned
bytes from a failed invocation carry a partial-capture warning; capture failures
must not be converted to empty target output.

The view is fixed on first successful collection for that source session/turn,
persisted atomically under session `feedback/`, with a hash and referenced
content-addressed blobs. Exact re-reads return that same view. Collect before
cleanup or another attempt/restore. The recording cutoff is the selected turn's
completion event. Include attributed setup facts through that boundary; exclude
explicitly different turns and later facts. Delivery without a turn binding is
labeled attempt-scoped and does not prove that a particular turn caused it.

Confirmed recorded applications/failures are normalized per injection ID with
source event sequence and scope. Preserve multiple events for an action. An armed
injection with no confirmation reports unknown, not not-matched or resisted.
Missing/invalid event recording makes those categories unavailable. A successful
invocation is not proof that an injection fired or an objective was achieved.

Oracle-assisted collection evaluates only attributed events through that cutoff
using supported event-backed detector kinds, including `database_query_match`.
Database outcomes use independent committed-state observations already recorded
during the requested turn and observed attempt. They are available before the
session ends; reading feedback never runs SQL against current database state.
Baseline/harness observations, other turns, later events and inherited cumulative
verdicts cannot substitute for this turn's observations. An observed outcome does
not prove that the attempt's injection caused it.

Feedback returns positive normalized outcomes (`oracle_id`, `oracle_kind`,
`fired`); no positive observation is not a negative verdict. Missing, incomplete
or ambiguous required database observations produce `oracle_evaluation_unavailable`
with an unavailable/partial category. Valid outcomes from other oracles can remain
available in a partial collection. Inspect category state as well as entries.
Other state-only detectors still require post-run evidence; they are not
re-evaluated against later mutable state. No raw query results, SQL, detector match
expressions or credentials are disclosed. The same profile/selection restrictions
apply to all oracle outcomes. Collection does not wait for asynchronous activity
after completion, and a retained receipt never changes after later writes or restore.

## Read actual bytes

Native `observation.content.read` uses the original session, campaign, attempt ID
and context digest in the normal envelope. Its body is:

```json
{
  "receipt_id":"feedback-<64 lowercase hex characters>",
  "entry_id":"entry-1",
  "offset":0,
  "max_bytes":262144
}
```

The response body repeats `receipt_id`, the immutable entry descriptor, offset,
base64 `content`, `raw_length` and `eof`. Read at most 256 KiB raw per call. Verify
ranges, metadata consistency and, after full assembly, the stored object's raw
SHA-256. EOF only means stored content ended; `entry.truncated` and the collection
state independently describe capture completeness. Decode text incrementally and
preserve unsupported binary content as opaque evidence.

Each stored artifact is at most min(16 MiB, max_blob_bytes). Larger captured
output stores a prefix and marks truncation/original size explicitly. Entry-cap
or content-storage failures mark the category and collection partial. Manifests
are bounded to 256 KiB. Publication failure returns 503 and does not substitute
another invocation or reopen execution. Operator separately enforces its shared
campaign read-byte/request budgets and logs delivered responses.

Invalid handles/ranges return 400, foreign receipt bindings 403, missing
receipt/entry 404, offset past stored size 416, and manifest/integrity/read failures
503. Missing/corrupt content behind a valid entry instead returns availability
unavailable with reason `content_missing_or_corrupt`, empty bytes and EOF false.
Offset equal to stored size returns zero bytes with EOF true for an available
artifact. Read errors do not dispatch target effects or close healthy execution.

## Retention and host/guest mapping

`observation.read` with `{"receipt_id":"..."}` retrieves an existing fixed view.
Both receipt and content reads work for a stopped/earlier source session after
its campaign association and native execution journal are verified. They never
restart a target or read replacement-session data. A stopped session cannot
collect a new view; clients must retain the original receipt. Missing files are
reported unavailable, including after administrative purge. Each session owns its
content; retained sessions do not depend on purged parent-session blobs. No
worker/revision ownership restriction is imposed.

Operator maps these native receipts/entries to its own opaque attempt-result
receipt and `engine.observation_read` entries. The guest has no general native
artifact.read operation or direct Interceptor access. Operator may further filter
and host-store bytes; it records that resulting descriptor and never grants access
by bare digest. The [shared Operator contract](../../operator_sandbox/schemas/FEEDBACK_CONTRACT.md)
defines the model-facing schemas, budget semantics and content presentation.

Capabilities advertise the feedback view version, selection modes/kinds, maximum
entries/chunk size, and observation.content.read operation. Existing capability
digests change. Regenerate source-bound manifests and deploy matching host binaries
and rebuilt supervisor images; do not silently mix old feedback clients with the
new view. Legacy retained target evidence remains available through its existing
reporting path, but no missing feedback receipt is fabricated from it.


Native model and service turn associations mean **observed during** the invocation,
not caused by it. When a request remains in flight at invocation completion, the
turn reports `evidence_partial: true`; its feedback does not claim full capture.
A later response keeps its original turn ID in exported events but does not
retroactively change a retained feedback receipt. Overlapping invocations have
ambiguous observations without a turn ID.

Required evidence-storage failure makes affected feedback partial/unavailable and
blocks new application invocations. Feedback inspection remains available when
its own storage is usable. Existing successful oracle results do not establish
that the affected turn was fully observed or that missing actions did not occur.
