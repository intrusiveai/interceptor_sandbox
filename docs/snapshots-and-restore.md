# Snapshots and restore

Interceptor snapshots are saved restore points. They let a tester return to a
known moment in an experiment or create multiple independent test branches from
that moment.

Interceptor does not copy an entire disk or freeze a running process. Instead,
it starts with the approved Environment Blueprint and records the meaningful
changes made during a session. The closest analogy is restoring a database from
a backup and its transaction history: begin with known starting data, then
reapply each saved change in order.

Internally, that ordered history is called the **state journal**. Larger changed
files are stored separately and referred to by their digital fingerprint. A
small **checkpoint** records exactly how far through the history a snapshot
includes.

This design is useful because the important question in an agent security test
is usually “can I reproduce and branch from this meaningful application state?”
rather than “can I reproduce every kernel page and open socket?” Logical state
is portable, can be inspected and checked for changes, and is substantially
simpler than copying a complete storage volume or freezing every detail of a
running process.

## What is captured

A checkpoint includes the persistent information needed to rebuild the synthetic
environment at that moment:

- records created, changed, or deleted in simulated stateful HTTP or MCP services;
- active synthetic identity grants, delegated-user permissions, and issuer
  failure settings;
- active injections, whether a one-time injection was already used, and
  response-selector ordinal progress;
- bounded resources and files created through fidelity injection;
- verified payload descriptors and immutable attempts registered before the
  checkpoint;
- files created, changed, linked, or deleted in folders declared as saved
  application state (`mutable_roots`);
- consumed usage limits, such as turns and model tokens;
- attempted snapshot count and cumulative canonical snapshot bytes;
- oracle results that had already fired;
- the configured virtual time; and
- fingerprints and version information needed to prove which application,
  environment, files, and history were used.

Changed file contents are stored once within the owning session, even if more
than one snapshot in that session uses the same content. The journal records
where each file belongs and refers to its content by digital fingerprint.

## What is not captured

A snapshot does not contain temporary details of the currently running process:

- information held only in application memory;
- running tasks, open files, locks, or active network connections;
- application caches that were not saved to declared storage;
- writes outside declared mutable roots;
- temporary filesystem contents;
- undeclared external service state;
- issued access tokens, projected token files, issuer private signing keys, and
  synthetic AWS session credentials;
- the model provider credential; or
- a promise that later stochastic model responses will be identical.

Restore always starts the application again as a fresh process. This is similar
to restarting an application after a clean shutdown: anything the application
needs afterward must have been saved to one of the declared persistent
locations.

## The journal and checkpoint

The state journal is a file containing one JSON change record per line. New
records are added to the end. Each record points back to the previous one, which
makes missing, reordered, or edited entries detectable. Interceptor writes a
managed service or injection change to this file before changing the live
environment, reducing the chance that saved history and live state disagree.

A checkpoint is a small JSON receipt containing:

- the associated campaign ID and an optional description of its purpose;

```json
{
  "schema_version": 1,
  "id": "cp-...",
  "label": "before-untrusted-input",
  "source_session_id": "sess-...",
  "parent_checkpoint": "",
  "environment_digest": "sha256:...",
  "app_digest": "sha256:...",
  "journal_seq": 12,
  "journal_hash": "sha256:...",
  "file_manifest_digest": "sha256:...",
  "canonical_size_bytes": 4096,
  "event_seq": 35,
  "created_at": "2026-08-25T12:00:00Z",
  "interceptor_version": "...",
  "journal_schema_version": 1,
  "status": "ready",
  "hash": "sha256:..."
}
```

The checkpoint becomes usable only after all other snapshot data has been saved.
Before restore, Interceptor checks the checkpoint's digital fingerprint and all
history positions it references.

## Snapshot creation

Create a snapshot with either interface:

```sh
interceptor snapshot create <session-id> --name before-untrusted-input
```

```sh
curl -sS -X POST "$CONTROL_URL/v1/snapshots" \
  -H 'Content-Type: application/json' \
  --data '{"label":"before-untrusted-input"}'
```

From the operator's point of view, Interceptor asks the application to save
pending work, confirms that its saved files have stopped changing, records the
changes, and then lets the application continue. The application is paused only
briefly near the end.

Under the hood, the coordinator:

1. temporarily prevents another administrative action or application request
   from changing state at the same time;
2. verifies that snapshots are enabled and within native count/byte limits and
   the optional caller byte allowance;
3. marks the session `quiescing` (preparing for a safe save);
4. asks the application to finish and save pending work using its configured
   quiescence method;
5. flushes the event recorder;
6. checks all saved application folders twice, 100 ms apart, and requires the
   same result both times;
7. briefly pauses the application for one final check;
8. determines which files changed since the last snapshot and saves their
   contents;
   Interceptor compares all canonical file bytes against the smaller of the
   remaining native allowance and optional caller `maximum_committed_bytes`,
   rejecting an oversized candidate with `429 snapshot_bytes_exhausted` before
   commitment (zero is exhausted, omission uses native limits);
9. records the file changes and other restorable state in the state journal;
10. saves the complete list and fingerprints of current files;
11. records current usage limits, oracle results, and virtual time;
12. writes the checkpoint and its digital fingerprint; and
13. records that the snapshot is ready, resumes the application, and returns
    the session to `running`.

If the application cannot finish saving or files continue to change between the
two checks, the request fails without creating a usable checkpoint. The running
session and earlier snapshots remain valid. Interceptor reports the problem
instead of producing a snapshot that might be incomplete.

File changes are collected when a snapshot is requested, not continuously.
Therefore, work performed after the latest successful snapshot can be lost if
the session crashes. The latest successful checkpoint is the latest state that
Interceptor promises it can rebuild.

## Restore

Use `POST /v1/lifecycle` on the integrated service; see the
[exact request contract](local-api.md#snapshot-and-restore). Restore retains the
host's original environment/image, provider and transport configuration. Those
credentials are absent from the checkpoint and remain in the live host process.
There is no independent `interceptor restore` launcher or concurrent branch
container in this MVP.

Before stopping the source or starting a replacement, restore preflight checks
checkpoint identity, sealed environment, journal linkage and required blobs.
The native startup path additionally checks application/image compatibility and
reconstruction. Failure after source closure is terminal, not a fallback to the
old target. Operator's cumulative accounting does not roll back with native state.

Before starting anything, restore verifies:Before starting anything, restore verifies:

- the checkpoint hash and source-session identity;
- the saved data format is supported by this Interceptor version;
- the Environment and application fingerprints still match;
- the currently available Sealed Environment and wrapper-image lock;
- the source journal's sequence and hash chain through the checkpoint; and
- every saved file body is present and unchanged.

It then creates a new session, copies the original starting files, and reapplies
saved changes in order. The new metadata records both `parent_session_id` and
`parent_checkpoint`, so post-run evidence can reconstruct the restore lineage.
More precisely, it:

1. creates a new session ID and private writable folder;
2. copies fresh starting files from the Sealed Environment;
3. creates a new event log and state journal;
4. reapplies successfully recorded service, identity, artifact, attempt,
   injection, budget, oracle, and clock changes through the checkpoint;
5. applies file operations to the new mutable roots;
6. checks that the rebuilt files have the fingerprint recorded by the snapshot;
7. records `restored_from` ancestry; and
8. starts a fresh container and application process, then waits for health.

The source is closed, stopped and removed before the new container starts.
Restored sessions retain checkpoint ancestry and own copies of the verified
journal prefix, required checkpoint/provenance records and every referenced blob.
Copies must be complete before readiness, so purging the source session cannot
break a retained replacement's verification, export or checkpoint restore. See
[file management](file-management.md) for cleanup and purge details.

A restored identity issuer creates a new signing key. Applications retrieve or
refresh new short-lived credentials, while the restored grants, delegated
consent, reduced scopes, revocations, and injected issuer failures remain as
they were at the checkpoint. This preserves the meaningful authorization state
without carrying an old or expired bearer credential into a new session.

## Branching and experimental value

Two deliberate restores with distinct operation IDs create sequential sessions
with the same checkpoint ancestry. An exact retry does not create a session. Changes
made in one do not affect the other. This enables controlled branches such as:

- restore “before injection,” apply payload A in one branch and B in another;
- preserve a post-compromise state, then test different follow-on inputs;
- hand a compact checkpoint and evidence chain to another investigator;
- return to an expensive setup state without replaying every preparatory turn;
- compare application versions against an equivalent synthetic state, provided
  an explicit compatible migration is available in a future schema; and
- reproduce whether a finding depends on transit simulation or persistent
  environment state.

Branching does not make later model sampling deterministic. To compare branches
fairly, control the provider/model settings and sampling parameters or use a
deterministic mock/recorded provider.

## Restoring an agent's conversation

A Python or Node [application state adapter](application-interfaces.md#saving-and-restoring-application-state)
can persist conversation context and session state alongside managed service
records and files. Interceptor packages the adapter through `app build`, asks it
to save at a completed-turn boundary, and includes its JSON file in the existing
integrity-checked snapshot. Restore loads that state into the newly initialized
application before readiness. The application keeps its logical conversation
identity while the sandbox receives a new native session ID.

This is not a process-memory snapshot or replay of the harness's interaction log.
The adapter must reconstruct the state that the application actually uses for
subsequent model calls and decisions. State not captured by the adapter or managed
files/services remains outside the restore guarantee. Snapshots do not restore
remote provider conversation state or renew old credentials by replaying them.

State-enabled adapters use one invocation at a time. New invocations wait during
capture; a hung export fails the session rather than continuing with uncertain
state. A recoverable export or size-limit error rejects the snapshot and permits
continued operation. Missing or incompatible state on restore fails startup.
Without state configuration, existing file-based application persistence and
snapshot semantics are unchanged.

## Conditions for truthful snapshots

Before setting `snapshot_capable: true`, verify all of the following:

- every value the application needs after restart is saved under a declared
  mutable root or in a managed stateful HTTP or MCP service;
- read-only and writable seeds are classified correctly;
- the configured quiescence action finishes buffered writes and background
  work;
- startup can reconstruct state without an earlier process;
- no required state lives only in `/tmp`, memory, or an external system;
- mutable roots do not overlap or contain system paths; and
- a full change → snapshot → restore test confirms that the application behaves
  correctly afterward, not merely that expected files exist.

If these conditions cannot be met, keep the application runnable with
`snapshot_capable: false`; that is more accurate than an incomplete snapshot.

## Campaign discovery and harness continuity

A harness can keep its conversation and hypothesis notes while repeatedly
restoring target checkpoints. Only the Interceptor target is replaced; Operator
advances the target revision and returns the result to the same live harness.
Known preflight rejection leaves both usable; post-closure failure/uncertainty
remains terminal. Restoring target state never resets campaign-wide budgets.

Create an administrative checkpoint with metadata before campaign attachment:

```sh
interceptor snapshot create <session-id> --name baseline \
  --campaign-id campaign-01 --description "Before mutation experiments" --json
interceptor snapshot list <session-id> --json
interceptor snapshot inspect <session-id> <checkpoint-id>
```

For an attached campaign, Operator uses native `snapshot.create` with host-supplied
`maximum_committed_bytes` (its remaining campaign allowance), label and
description; campaign ID comes from its request envelope. Description is optional,
valid UTF-8 and at most 4,096 bytes. Both fields are stored in the checkpoint,
covered by its hash and returned by create/list/inspect. Older records may omit
them and remain readable through verified source-session membership.

The [local list/inspect API](local-api.md#snapshot-inventory-and-metadata) lets
Operator discover checkpoints from current and earlier retained campaign sessions,
including after stop. Use both source session and checkpoint ID to inspect or
restore an earlier baseline. The harness receives a safe metadata projection,
including campaign ID and description, through Operator tools; it never reads
raw checkpoint storage. Restoring a checkpoint does not edit its metadata.
