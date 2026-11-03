# Application state adapters for snapshot restore

## Outcome and scope

Interceptor MUST support restoring an application's conversation context and
session state through an application-specific adapter supplied with the
Environment Blueprint and packaged by `interceptor app build`. The application
MUST NOT need a framework checkpointer or source modifications to use this
integration, provided the adapter can access and reconstruct all required state.

After restoring a snapshot, the next invocation MUST use the saved conversation,
logical application session identity, and relevant application state together
with the restored sandbox environment. Subsequent turns MUST continue updating
that restored state normally. This is explicit application state persistence,
not process-memory capture or injection of history into model requests.

The implementation MUST support the existing Python and Node `code` interfaces,
including synchronous and asynchronous adapter methods. It MUST retain current
behavior for configurations that omit state adapters and for applications that
already restore themselves from declared files.

Snapshots MUST occur between completed invocations. Mid-invocation execution
resumption, automatic discovery of arbitrary object state, framework-specific
plugins, provider-side conversation recovery, and new external storage services
are outside this feature. PostgreSQL emulation is a separate requirement.

## Existing mechanisms to reuse

- `internal/environment` defines, validates, and seals Blueprint configuration
  and adapter assets.
- `internal/appimage/adapter.go` and `internal/appimage/appimage.go` package the
  sealed adapter directory into the wrapper without executing it during build.
- `internal/sandboxd/application_adapter.go` and its embedded Python and Node
  drivers own the application object and its invocation protocol.
- `internal/sandboxd/server.go` coordinates quiescence and snapshot capture;
  `internal/snapshot` and `internal/localapi/snapshots.go` persist declared files
  and reconstruct them before starting the replacement application.

The implementation MUST extend these mechanisms rather than introduce another
adapter distribution system, checkpoint service, manifest, or public snapshot
API. Review actual callers, admission paths, and startup ordering before editing;
the snapshot lock alone MUST NOT be assumed to cover direct application calls.

## Blueprint and adapter contract

Add the optional `application.interface.code.state` mapping:

```yaml
application:
  # Existing image and launch configuration remains required.
  interface:
    kind: code
    operations:
      - id: ask
    code:
      language: python
      adapter_asset: assets/adapter
      module: support_adapter
      initialize: initialize
      invoke: invoke
      close: close
      concurrency: 1
      state:
        save: save_state
        restore: restore_state
        path: /app-state/agent.json
  persistence:
    snapshot_capable: true
    mutable_roots:
      - /app-state
    quiesce:
      kind: none
      timeout_ms: 5000
```

This is a configuration fragment. Runnable examples MUST supply a complete
Blueprint. Node MUST use the equivalent mapping with its existing language and
module conventions. `quiesce.kind: none` means no additional application flush
action; it MUST NOT skip the configured state save operation.

When `state` is present:

- `save`, `restore`, and `path` MUST all be required, and snapshot capability
  MUST be enabled. Unsupported interface configurations MUST be rejected.
- The methods MUST resolve on the same object used for invocation: the return
  value of `initialize`, or the imported module when no initializer is set.
  An already packaged module MAY implement the same contract without a separate
  `adapter_asset`.
- `save_state()` MUST return a JSON object describing the complete state required
  for continuation. `restore_state(state)` MUST apply that object to the current
  application instance; its return value is ignored. Both methods MUST be
  validated as callable before readiness.
- The driver MUST await asynchronous methods and preserve the existing event
  loop and object lifetime. It MUST NOT instantiate a second agent to save state.
- `path` MUST be an absolute container file path below a declared mutable root,
  outside installed code, adapter assets, protected control paths, and configured
  attacker-writable file destinations. Path traversal and symlink escapes MUST
  be rejected. Use existing path and injection-policy validation where possible.
- State-enabled adapters MUST use concurrency `1` in this implementation;
  reject higher concurrency explicitly. Existing Node concurrency support MUST
  remain unchanged when `state` is absent.
- Configuration and adapter assets MUST participate in the existing seal and
  wrapper identity. Changes MUST require resealing and rebuilding as they do for
  invocation adapters. No new compatibility registry is needed.

The state methods MUST execute inside the application's existing process and
under its existing permissions. They MUST NOT receive host credentials or
additional network access. Build MUST package the supplied code without running
it, modifying the original image, or installing dependencies at runtime.

## Adapter responsibilities and behavioral fidelity

An adapter MUST explicitly inventory and serialize all state needed to continue
the supported application. This can include:

- Ordered messages, roles, structured content, tool-call identifiers and results,
  summaries, and other context used to construct subsequent model requests.
- Logical conversation/session identity and its authenticated user association.
- Counters, pending restrictions, or safeguards that influence future behavior.

The adapter MUST reconstruct application invariants and object relationships,
not just assign a transcript. It MUST recreate transient locks, clients, and
connections rather than serialize them. Any required state outside this inventory
MUST be covered by the existing managed service/file snapshot or identified as
unsupported. A provider-side identifier alone is not a restorable context window.

For the customer support application's current structure, an integration must
account for the `Session` messages, customer ID, session ID, uncertain-refund and
failed-interaction sets, and the application's session registry. A newly created
lock is appropriate; a new logical session ID or a missing registry entry is not.
This inventory MUST be checked against the actual application version. The
feature MUST NOT modify that sibling repository or require PostgreSQL support.

The adapter MUST preserve prompts, tool definitions, message order, truncation
policy, identity checks, and safeguards. Save/restore MUST NOT invoke the model,
replay earlier inputs or tool calls, or perform business operations to rebuild
history. Initialization MUST also avoid such side effects on restored startup;
an application that cannot be initialized safely is unsupported until its adapter
can provide a safe reconstruction path.

Save MUST be repeatable and leave the live agent's logical state unchanged.
Background tasks MUST NOT mutate snapshot-relevant state between export and
snapshot completion. An integration MUST use existing quiescence facilities to
establish that condition or declare itself unsupported; invocation serialization
alone does not stop background work.

Application-specific payloads SHOULD contain their own format version and
validate required fields and types during restore. Unsupported payloads MUST
fail rather than silently reset or omit state. Do not add automatic migrations
or introspection of private fields to Interceptor itself.

## Save lifecycle and persisted representation

1. Snapshot coordination MUST prevent new invocations through every supported
   invocation route and wait for existing work to finish. Waiting MUST respect
   cancellation and bounded lifecycle timeouts. Quiescence and state export
   MUST NOT race with invocation, close, or another export.
2. After the configured quiescence action completes, the supervisor MUST request
   state export through the existing private application protocol. The driver
   MUST call the configured save method and write the state file before returning
   a correlated success acknowledgement. This is an internal control operation,
   not a new application operation, user turn, or tool call.
3. The driver MUST encode one JSON document with the shape
   `{"version": 1, "state": <application state object>}`. Serialization MUST reject
   unsupported or lossy values, including non-finite numbers, cycles, and values
   that would otherwise be silently dropped or converted to strings. Adapters MUST explicitly encode
   framework message objects and other non-JSON types. Do not reuse the Python
   invocation result's `default=str` fallback or use pickle/equivalent object
   deserialization.
4. The document MUST be size-bounded using the existing blob limit and written
   atomically using a temporary file in the same directory, flush, replacement,
   and required durability synchronization. Partial files MUST NOT replace a
   valid saved state. Parent directory creation and file permissions MUST work
   with the existing application UID and read-only container filesystem.
5. Only after successful export may the existing stable-filesystem checks,
   process pause, journal, blob, and checkpoint sequence proceed. The state file
   MUST be covered by existing integrity verification and snapshot byte budgets.
   There MUST NOT be a separate state manifest or independent restore point.
6. A snapshot MUST become usable only when both the environment state and the
   corresponding application state are durably captured. Invocation admission
   MUST reopen after successful completion or a recoverable snapshot failure.

Only metadata needed to diagnose lifecycle failures SHOULD be recorded in control
events. The implementation MUST NOT duplicate the state payload into stdout,
protocol diagnostics, or control events. State files remain application data
covered by existing snapshot access and export rules; they are not a new secret
store or trusted evaluation evidence.

## Restore lifecycle

The supervisor MUST derive whether startup is a restore from existing verified
session/checkpoint metadata and convey it to the driver through supervisor-owned
configuration. The presence of a state file alone MUST NOT select restore mode.
Application launch environment overrides MUST NOT override this decision.

On restored startup, Interceptor MUST:

1. Verify and reconstruct the selected snapshot's managed environment and files
   using the existing restore path.
2. Initialize the configured adapter/application, validate its methods, and read
   the bounded state document from the configured path.
3. Validate the document version and structure, then call and await the restore
   method on the invocation object.
4. Announce readiness and permit invocation only after successful restoration.

A fresh session MUST initialize normally and MUST NOT load a checkpoint merely
because a file exists at the configured path. Restoring a snapshot of an empty
conversation MUST still restore an explicit saved state; missing state is not an
empty conversation. Initializing transient clients or locks MUST NOT overwrite
the restored state file.

The application session identity MUST survive while Interceptor continues to
assign a new native sandbox session ID and preserve restore ancestry. Restored
conversation state MUST belong to the selected checkpoint, not the latest state
of the source process or another session. Existing credential renewal and issuer
key behavior MUST remain unchanged; saved identity MUST NOT be used to revive
expired credentials or bypass authorization.

## Failures and compatibility

- Missing methods, invalid configuration, and unsafe state paths MUST produce
  actionable validation or startup errors identifying the affected setting.
- Save exceptions, invalid JSON state, write failures, size exhaustion, and
  unconfirmed saves MUST fail snapshot creation without publishing a checkpoint
  or reusing a stale state file as if export had succeeded.
- A recoverable save rejection MUST preserve existing usable snapshots and the
  live session. A timeout or broken protocol that leaves application execution
  uncertain MUST follow existing adapter failure/termination behavior; it MUST
  NOT report a healthy session while a save may still be running.
- A missing, corrupt, incompatible, or rejected restored payload MUST fail
  restored startup before readiness. There MUST be no fallback to fresh history,
  model-request rewriting, or transcript replay.
- Existing snapshot rollback, integrity, budget, cancellation, and isolation
  behavior MUST remain supported. Ordinary applications without state adapters
  MUST retain their existing startup and snapshot semantics.
- Interceptor MUST NOT infer full snapshot support from a framework name or the
  presence of configured methods. The integration must be verified to capture
  everything its application requires.

## Implementation and documentation

The implementation MUST update the Blueprint types, validation, and sealing
tests; application protocol and lifecycle coordination; both embedded language
drivers; and snapshot/startup integration tests. Reuse optional adapter behavior
without requiring every existing interface to implement state methods.

Provide runnable Python and Node examples under
`examples/application-state-snapshots/`, with target source, Dockerfiles, complete
Blueprints, externally supplied adapters, and usage instructions. The target
applications MUST hold their conversation/session state in memory and MUST NOT
contain a persistence implementation or framework checkpointer. Adapter code
MUST supply the persistence without rewriting the target's source.

Update `docs/application-interfaces.md`, `docs/application-integration.md`, and
`docs/snapshots-and-restore.md` to document the configuration, adapter contract,
writable storage requirements, lifecycle, failure behavior, and application
compatibility limits. Align the corresponding contracts in `specs/mvp.md` and
link this extension from `specs/README.md`. Documentation MUST distinguish saved
application context from process memory and from an observer's interaction log.
It MUST describe the adapter as part of the tested configuration without claiming
that the unchanged application itself has production restart support.

## Verification and acceptance

Focused automated tests MUST cover:

- Configuration defaults, all required fields, method resolution, sealing and
  packaging, path containment, disallowed file-injection overlap, and unchanged
  invocation-only adapters.
- Python sync/async methods and Node sync/async methods, object identity and
  event-loop lifetime, exact JSON round trips, and strict serialization failures.
- Fresh versus restored startup, an empty saved conversation, missing/corrupt/
  incompatible payloads, method exceptions, timeouts, and restore before readiness.
- Admission through both control and direct invocation routes, snapshot during
  an active turn, cancellation, and prevention of concurrent export/invocation.
- Atomic-write failures, blob/snapshot limits, stale-file rejection after failed
  export, existing snapshot integrity checks, and recovery after a safe failure.
- Multiple successive restores and isolation between application sessions.

A complete Docker qualification MUST build wrappers from the supplied examples
using the normal seal/build/start commands and exercise both languages. It MUST:

1. Run several turns that establish conversation facts and identity, perform a
   simulated tool operation, and change a managed service record and a declared
   application file.
2. Capture a snapshot. Invoke a chosen next input and record its model request
   and resulting state as the uninterrupted baseline. Run further turns that
   alter both agent context and environment state.
3. Restore the snapshot into a new application process and check that its state
   matches the saved boundary before accepting another invocation.
4. Invoke that input after restore and compare the outgoing model request with
   the uninterrupted baseline: messages, tool history, instructions, and logical
   identity MUST agree. Any normalized transient fields MUST be explicitly
   justified; do not normalize away conversation or identity differences.
5. Verify that service/file state and application safeguards match the selected
   checkpoint, post-snapshot facts are absent, and old tool side effects were not
   replayed. Run at least two further turns to demonstrate continuing memory.
6. Verify target source/image identity is unchanged and adapters are included in
   the sealed wrapper identity. Clean up test sessions and containers on success
   and failure.

Use deterministic model/tool responses for reproducible assertions while routing
real model requests through Interceptor's normal bridge and recording path.
Different live model output is not itself a restore failure; the restored inputs
and relevant state are the comparison target. A transcript-only unit test does
not establish end-to-end support.

Extend the existing packaged-runtime qualification in
`scripts/qualify_release.py` and `.github/workflows/verify-release.yml` to exercise
this workflow using the CI-produced release archive. Preserve archive verification
and existing publication conditions. Run applicable Go tests, driver tests, and
Docker qualification; report actual platforms and any unavailable validation.
The customer support agent MAY be additionally qualified when its separate
dependencies are available, but substitute examples MUST NOT be reported as
qualification of that application.
