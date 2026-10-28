# Observed turn attribution and evidence-storage failures

## Outcome and scope

Interceptor MUST independently associate native model and managed-service
observations with the application invocation during which they were received.
Public descriptions MUST say **observed during a turn**, not caused by that
turn. Interceptor MUST explicitly report when required evidence cannot be
preserved and MUST stop admitting additional turns in that session.

These corrections MUST work without target source changes, application-supplied
correlation headers, HTTP-client patches, or collection of agent-authored logs.
They MUST cover control API invocations and direct application entry points.
PostgreSQL emulation, causal tracing of background tasks, and changes to
application state adapters are outside scope.

## Existing mechanisms to reuse

The implementation MUST inspect and extend the existing components:

- `internal/sandboxd/application_adapter.go`, its drivers, and
  `internal/sandboxd/server.go`: invocation admission, execution, completion,
  model/service routing, session lifecycle, and snapshot coordination.
- `internal/bridge/proxy.go`: model request/response capture and injection
  processing, including the existing embeddings storage-error handling.
- `internal/services`: fixture, stateful, MCP, sink, and response-injection
  observations; `internal/statejournal`: committed service mutations.
- `internal/recording` and `internal/blobstore`: native events, integrity, blob
  storage, and flush boundaries.
- `internal/sandboxd/feedback.go`, `internal/evaluation`, and the existing
  local API/export paths: partial/unavailable feedback, oracle results, and
  inspectable evidence.

The implementation MUST reuse the event stream, session metadata, and existing
error/partial-result mechanisms. It MUST NOT introduce a separate evidence
service, manifest system, application instrumentation framework, or storage
recovery subsystem. Any additive public fields MUST have defined semantics and
be reflected in the applicable contracts and consumers.

## Supervisor-owned observation windows

### Invocation boundaries

1. Interceptor MUST register an invocation window immediately before actually
   handing work to the application, after acquiring applicable adapter capacity.
   Time waiting in an invocation queue MUST NOT count as active execution.
2. The window MUST use the invocation identifier already assigned by Interceptor.
   It MUST include the admitted attempt context when one exists; direct requests
   MUST NOT invent an adaptive attempt identity.
3. Interceptor MUST close the window when the invocation returns, errors, or is
   cancelled. Cleanup MUST occur on every exit path. Closed windows MUST NOT be
   reused after snapshot restore or application restart.
4. Cancellation MUST preserve the existing application's termination/cancellation
   contract. A timed-out invocation MUST NOT remain the active turn indefinitely;
   if application execution remains uncertain, that uncertainty MUST NOT become
   trusted attribution to a subsequent turn.

All invocation paths MUST share these semantics: HTTP, command, REPL/stdio,
external protocol, and Python/Node code interfaces. Existing concurrency settings
MUST remain supported. The implementation SHOULD use a small synchronized
supervisor-owned registry rather than add a new Blueprint option.

### Binding observations

At entry to each native model or managed-service request handler, Interceptor
MUST capture an immutable observation association from the registry:

- Exactly one invocation is executing: associate the request with that window.
- No invocation is executing: retain it as a session observation without a turn.
- Multiple invocations are executing: retain it without a turn and identify the
  association as ambiguous. MUST NOT select the newest, oldest, or caller-claimed
  turn.

The association MUST be captured before body processing, queuing, or downstream
work can move the request into another window. Events derived from the request,
including its response or committed mutation, MUST retain that association even
if they finish after the window closes. Responses MUST NOT consult a new active
turn. Parallel model/tool requests within one invocation MAY share its window.

The existing `turn_id` field SHOULD carry the observed window association, with
minimal native metadata distinguishing supervisor observation from legacy or
caller-supplied attribution and explaining absent/ambiguous associations.
Association metadata MUST be integrity-covered with the event. Existing event
files MUST remain verifiable without rewriting their contents or hashes.

The implementation MUST remove the previous `X-Interceptor-Turn-ID` attribution
mechanism entirely. Interceptor MUST stop injecting that header into HTTP
application invocations, including direct application requests, and MUST remove
all header-reading logic used for evidence association or turn-specific injection
selection. It MUST NOT retain a compatibility mode or header-based fallback.
Supervisor-owned observation windows MUST be the sole mechanism for associating
application-originated model and service requests with an executing turn.

Caller-supplied copies of the old header MUST have no effect on observation
association or injection selection, including when no window exists or windows
overlap. Obsolete propagation helpers, configuration, examples, and tests that
require header-based attribution MUST be removed or updated. Internal invocation
IDs and protocol-frame IDs MUST remain; removing the HTTP header MUST NOT remove
the identifiers needed to match application results to their invocations.

The implementation MUST distinguish the attempt during which a request was
observed from the identity of an injection that affected it. Existing injection
IDs, origin attempt IDs, and payload digests MUST NOT be overwritten to force
them to match an observation window. Feedback MUST retain their distinct
meanings and preserve existing visibility restrictions.

### Meaning and limits

An association establishes the invocation active when Interceptor received the
request. It MUST NOT establish causal provenance or prove that the invocation
initiated a background task. Background activity can occur inside another
invocation's window; Interceptor cannot distinguish that using timing alone.
Applications whose work completes before returning provide the clearest
turn-level interpretation. Startup, shutdown, state-save/restore hooks, and
between-turn activity MUST NOT create synthetic application-turn associations.

Evaluation, injection delivery reporting, documentation, and exported evidence
MUST consistently describe these as observations. A temporal association alone
MUST NOT satisfy an existing causal-injection requirement or establish that a
claimed business action committed. Service state/journal evidence remains the
independent basis for committed-state assertions.

Existing cumulative/session-level oracle semantics and latched results MUST
remain intact. A previously fired oracle MUST NOT be described as proof of a
new turn's effect.

Feedback captured at turn completion MUST remain a bounded view. A response
recorded later with that turn association MUST NOT be reassigned to the next
turn or silently inserted into an already-issued receipt. If observed requests
are still outstanding, feedback MUST disclose that its capture is partial.
Later evidence MUST remain inspectable through existing evidence/export paths.
The implementation MUST NOT introduce indefinite waits for background work.

## Required evidence-storage failure policy

### One failure path

The implementation MUST provide a shared, concurrency-safe path for reporting
required evidence failures. It MUST cover blob writes, event appends, short or
partial writes, required flushes, and required state-journal writes. Existing
unhandled recording errors in the covered model, service, injection, and turn
paths MUST be audited and corrected.

Required evidence is the native record and content needed by the operation's
documented capture and evaluation contract. Optional diagnostic logging MUST NOT
become fatal merely because it uses a file. Intentionally omitted content MUST
be distinguishable from content lost through a failed write. Production paths
MUST NOT silently disable required collection because a recorder is unavailable.

On the first required evidence failure, Interceptor MUST:

1. Latch the failure in supervisor memory, including a stable error code and
   safe operation/turn context where available.
2. Mark session evidence incomplete through existing session status/taint and
   feedback mechanisms, persisting this state when possible.
3. Reject additional application invocations on every admission path. Requests
   already queued MUST recheck the failure before entering the application.
4. Return an explicit evidence-storage error to affected callers and prevent an
   affected invocation from being reported as successfully captured, even if the
   application swallowed an individual service/model error.

The latch MUST remain set for the session. Further failures MUST NOT replace the
original cause with a misleading application-exit or cleanup error. Error
reporting MUST NOT recursively depend on appending an event to the failing store.
Metadata persistence failure MUST be surfaced through the live control response;
the implementation MUST NOT claim that failure status was durably saved when it
was not. After storage repair, recovery uses a new session.

### Ordering and side effects

- Before forwarding a model request or executing a service operation, required
  request evidence MUST be successfully stored according to the existing
  persistence contract. Failure MUST prevent that operation from being sent or
  executed. Reuse existing request events/blobs where available; add only the
  minimum record necessary where the current contract cannot express this.
- After a provider call or service mutation has occurred, failure to preserve
  its result MUST report incomplete evidence and a possibly completed operation.
  Interceptor MUST NOT claim rollback, synthesize a negative result, or retry the
  business/provider operation automatically.
- Required response/outcome recording MUST complete before sending a success
  response. Required turn-output capture, completion append, and flush MUST
  complete before reporting a successfully captured turn.
- All generation and embedding routes MUST follow the same policy. Fixture,
  stateful, MCP, sink, and injected-response observations MUST follow it too.
- Admission and failure transitions MUST be race-safe. Work already in flight
  MAY have effects and MUST retain its original association. The implementation
  MUST NOT claim to undo it. Further dependent operations MUST be rejected after
  the failure is latched; inspection and cleanup MUST remain possible.

The implementation MUST preserve successfully stored evidence and existing
integrity checks. It MUST NOT truncate a corrupt stream or fabricate replacement
records to make verification succeed. Partial-write handling MUST prevent later
appends from presenting an invalid chain as healthy.

### Evaluation, snapshots, and inspection

Missing required evidence MUST NOT produce an ordinary negative oracle result,
`DELIVERED_RESISTED`, or an assertion that an action did not occur. Affected
evaluation MUST return an explicit error or use existing unavailable/partial
states. A conservative session-wide evaluation error is acceptable; selective
dependency tracking for individual oracles is not required.

Previously established results MUST remain inspectable with their original
scope, but MUST NOT hide the incomplete status of the affected turn/session.
New snapshots after the failure MUST be rejected rather than presented as
complete restore points. Previously completed snapshots MUST retain their
original guarantees.

Status, feedback, and export operations MUST expose the failure when available.
Healthy recorded material SHOULD remain exportable with explicit incomplete
status. If storage corruption prevents normal export, the operation MUST fail
explicitly and preserve the underlying files for diagnosis. No new recovery
archive format is required. Hash verification alone MUST NOT be described as
proof that collection was complete.

## Verification

Focused tests MUST cover:

- Sequential turns with multiple model calls and managed-service reads/mutations,
  without application correlation headers; all derived events retain the correct
  observed association and available attempt context.
- HTTP application invocations through both control and direct entry points no
  longer receive an Interceptor-generated turn header. Caller-supplied old
  headers cannot change attribution or select turn-specific injections, with or
  without an active observation window.
- Forged headers, no active invocation, overlapping invocations, queued work,
  cancellation, and application errors. Ambiguous activity remains observable
  without a guessed turn; existing concurrency behavior is preserved.
- Parallel requests within one turn and a response finishing after that turn
  ends. Completion-time feedback discloses pending capture, and late records
  never acquire a subsequent turn's identity.
- Background requests during a turn receive only observed-window semantics;
  requests between turns, at startup, and during state save/restore remain
  outside application turns. No causal conclusion follows solely from timing.
- Control API and direct application admission, Python and Node code invocation,
  HTTP invocation, and the remaining supported adapter paths. Snapshot/turn
  locking MUST remain free of service-call deadlocks.
- Fault injection at request storage, response storage, turn-output storage,
  event append, short write, flush, and service journal boundaries. Tests MUST
  verify prevented calls before execution and truthful uncertainty after effects.
- Failures swallowed by application code, concurrent in-flight calls, queued
  invocations, and failure-status persistence itself. New work is rejected and
  the original failure remains visible without recursive logging.
- A missing evidence reference cannot become a negative oracle/resisted result;
  previously fired results do not conceal incompleteness. Healthy historical
  event files still verify, and partial records are not silently repaired.

The implementation MUST also demonstrate the full workflow in Docker using an
existing packaged application example and native qualification tools. The
example MUST make sequential invocations with model and managed-service calls
without forwarding turn headers, export the native evidence, and verify its
associations. It MUST then exercise a controlled evidence-storage failure and
show the caller-visible error, rejected next invocation, and incomplete
evaluation/feedback. Use a deterministic host model fixture; do not require a
live model provider or target source changes for correlation.

Fault controls MUST remain test-only. Verification MUST inspect actual exported
events and preserved service effects, not just mocked callbacks. Created
resources MUST be cleaned up. Run relevant unit/race tests and existing native
qualification checks; report actual platforms exercised and any unavailable
end-to-end validation explicitly.

## Documentation and completion

Update the applicable native evidence, application interface, feedback,
injection/evaluation, and snapshot documentation plus their normative contracts.
Document observed-window semantics, concurrency/background limitations, removal
of header-based attribution, required-storage failure behavior, and how to
inspect an incomplete session. Remove instructions to supply or propagate
`X-Interceptor-Turn-ID`; examples MUST use supervisor-owned attribution. Keep
causal conclusions and session-cumulative oracle results distinct from turn
observations.

The completed change MUST include the runtime behavior, focused regression
tests, representative end-to-end validation, and user-facing documentation.
Agent-authored evidence collection or correlation MUST NOT be added as an
integration requirement.
