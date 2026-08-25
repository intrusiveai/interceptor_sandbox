# Snapshots and restore

Interceptor snapshots are logical restore points, not disk clones and not
process checkpoints. The closest analogy is a database backup plus transaction
log: the Sealed Environment is the known base, the state journal is the ordered
change record, content-addressed blobs hold changed file bytes, and a checkpoint
pins the exact committed position to restore.

This design is useful because the important question in an agent security test
is usually “can I reproduce and branch from this meaningful application state?”
rather than “can I reproduce every kernel page and open socket?” Logical state
is portable, inspectable, integrity-verifiable, and substantially simpler than
volume snapshots, copy-on-write filesystems, CRIU, or infrastructure-specific
cloning.

## What is captured

A checkpoint covers state committed through one journal position:

- mutations to `stateful_http` resources;
- injection definitions, deletion, consumption, and delivery state;
- filesystem creates, updates, symlinks, and deletes under declared
  `mutable_roots`, captured as deltas at snapshot boundaries;
- consumed turn/token/snapshot budgets;
- sticky oracle state;
- the fixed virtual clock state;
- Environment and application digests;
- final mutable-filesystem manifest digest; and
- the source event-log position and tool version/schema identity.

Changed regular-file bodies are deduplicated in the shared SHA-256 blob store.
The journal refers to their digest, size, mode, and absolute container path.

## What is not captured

A snapshot does not contain:

- process memory, threads, stacks, or CPU state;
- open files, locks, sockets, or active HTTP connections;
- application caches that were not externalized;
- writes outside declared mutable roots;
- temporary filesystem contents;
- undeclared external service state;
- the model provider credential; or
- a promise that later stochastic model responses will be identical.

Restore always starts a fresh application process. A snapshot-capable
application must be designed to reconstruct all required runtime state from the
captured persistent contract.

## The journal and checkpoint

The state journal is append-only JSONL. Each transaction contains a sequence,
reason, ordered operations, previous hash, and hash. Managed service and
injection mutations use write-ahead semantics: the transaction is synced before
the live state is changed.

A checkpoint is a small JSON manifest containing:

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
  "event_seq": 35,
  "created_at": "2026-08-25T12:00:00Z",
  "interceptor_version": "...",
  "journal_schema_version": 1,
  "status": "ready",
  "hash": "sha256:..."
}
```

The checkpoint is published last. Its hash and all referenced chain positions
must verify before restore.

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

The coordinator:

1. verifies that snapshots are enabled and within the configured limit;
2. serializes control work and blocks new application/service activity;
3. marks the session `quiescing`;
4. runs the declared application quiesce protocol;
5. flushes the event recorder;
6. scans all mutable roots twice, 100 ms apart, and requires identical digests;
7. pauses the application process group for the final scan;
8. computes the delta from the previous file manifest and stores changed bodies
   as blobs;
9. appends filesystem operations and snapshot state to the durable journal;
10. writes the current file manifest;
11. evaluates and captures budget, oracle, and clock state;
12. writes the hashed checkpoint; and
13. records `snapshot_ready`, resumes the application, and returns the session
    to `running`.

If quiescence fails or the two scans differ, the request fails without
publishing a checkpoint. Existing state and earlier committed journal entries
remain valid. Interceptor does not pretend that an unstable boundary succeeded.

Filesystem writes between snapshots are not continuously journaled. Therefore
the mechanism is not crash recovery for arbitrary unsnapshotted writes. The
most recent published checkpoint is the recovery boundary.

## Restore

```sh
interceptor restore <source-session-id> \
  --checkpoint <checkpoint-id> \
  --provider-base-url https://provider.example/v1 \
  --allow-model provider-model-id
```

The host provider options are required again because credentials and provider
configuration are deliberately absent from snapshots.

Before starting anything, restore verifies:

- the checkpoint hash and source-session identity;
- exact Interceptor and journal schema compatibility;
- Environment and application digests;
- the currently available Sealed Environment and wrapper-image lock;
- the source journal's sequence and hash chain through the checkpoint; and
- every referenced filesystem blob.

It then:

1. creates a new session ID and new generation directory;
2. materializes fresh writable seeds from the Sealed Environment;
3. creates a new event log and state journal;
4. replays committed managed-state operations through the checkpoint into that
   new journal;
5. applies file operations to the new mutable roots;
6. rescans and requires the resulting manifest digest to match the checkpoint;
7. records `restored_from` ancestry; and
8. starts a fresh container and application process, then waits for health.

The source is unchanged by default. `--stop-source` stops it only after the new
session becomes healthy.

## Branching and experimental value

Restoring the same checkpoint twice creates two independent sessions and future
journals. This enables controlled branches such as:

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

## Conditions for truthful snapshots

Before setting `snapshot_capable: true`, verify all of the following:

- every durable write is under a declared mutable root or managed stateful
  service;
- read-only and writable seeds are classified correctly;
- the quiesce hook flushes buffered writes and background workers;
- startup can reconstruct state without an earlier process;
- no required state lives only in `/tmp`, memory, or an external system;
- mutable roots do not overlap or contain system paths; and
- a golden mutate → snapshot → restore test checks actual application behavior,
  not just file existence.

If these conditions cannot be met, keep the application runnable with
`snapshot_capable: false`; that is more accurate than an incomplete snapshot.

