# Private mutable-file snapshots

Native snapshots MUST capture and restore files owned by Interceptor's application
identity inside declared mutable roots, including nested directories with mode
0700 and files with mode 0600. Applications MUST be able to read and update the
restored files. Mixed supervisor-owned seed files and application-owned files MUST
retain their respective owners and modes. Application code and Operator's
snapshot API/workflow MUST remain unchanged.

## Capture

- Extend the existing application-identity helper protocol with bounded filesystem
  reads. Use a separate helper process group so reads work while the target's
  process group is paused, including command targets whose main process is itself
  an application helper.
- The helper MUST read only declared mutable roots. Preserve symlink containment,
  no-follow checks, hard-link rejection and unsupported-file-type rejection.
  Requests, directory pages, file chunks and aggregate capture MUST be bounded.
- The supervisor MUST validate returned metadata and paths, store file blobs,
  enforce per-blob and remaining snapshot byte limits, check filesystem stability,
  and commit the checkpoint. The helper MUST NOT receive access to supervisor
  snapshot storage. Inaccessible entries MUST fail capture rather than be skipped.

## Ownership and restore

- File manifest schema 2 MUST record each entry's logical `application` or
  `supervisor` owner and permission bits, including mutable root directories.
  Numeric host UIDs MUST NOT become portable snapshot identities. Other owners
  and groups MUST be rejected. ACLs and arbitrary OS accounts are out of scope.
- Manifest digests and filesystem journal operations MUST cover ownership and
  modes. Ownership-only changes MUST produce a journal delta. Existing journal
  records with absent ownership MUST remain hash-verifiable for inspection.
- New session materialization records MUST identify the ownership-aware file
  manifest schema. Legacy checkpoints lacking this marker MUST be rejected during
  restore preflight with guidance to create a new session/checkpoint; ownership
  MUST NOT be guessed. Legacy evidence remains readable.
- Restore MUST reconstruct the expected logical manifest from the sealed baseline
  and verified journal, and match the checkpoint digest before starting a target.
  Host staging MUST use restrictive host-owned files and directories, without
  requiring host root privileges or applying arbitrary numeric ownership.
- Container bootstrap MUST verify staged file contents and the complete tree,
  then apply destination identity ownership and exact recorded modes before any
  application process starts. Restrict temporary CHOWN, FOWNER and DAC_OVERRIDE
  capabilities to mutable-root bootstrap. Fresh volume initialization MUST
  establish the seed manifest's supervisor ownership. Drop them when adopting
  the supervisor identity, before launching helper or application processes.
- Fresh-environment initialization MAY continue preparing writable seed files.
  Restored state MUST bypass that permission-widening step.

## File-state oracles

- `file_state_match` MUST reuse the restricted filesystem reader for application
  private files. The reader MUST be available even when snapshots are disabled
  and remain available until final oracle evaluation finishes at shutdown.
- Only a confirmed not-found error MAY satisfy an absence predicate. Permission
  failures, reader failures, cancellation and unsafe paths MUST fail evaluation,
  rather than produce a verdict based on missing data. Preserve error classes
  across the helper protocol. Content reads MUST enforce the existing blob limit.
- Existing oracle verdict retention, evidence storage, and evaluation timing MUST
  remain unchanged. No new administrator or Blueprint permission settings are
  required. Oracle records remain supervisor-owned; the helper only reads files.
- Tests MUST cover private content, existence, digest and nested absence predicates
  before capture, after baseline reset, and after restore and update. Verify
  inaccessible-file errors, containment checks, and complete final evidence.

## Mutable-root storage

- Linux and macOS mutable roots MUST use session-owned Docker local volumes.
  This preserves ownership on Docker Desktop and lets ordinary session purge
  remove private files without requiring host permission repair.
- Each mutable root MUST have a separately named local volume, mounted without
  Docker's automatic image-content copying. Before the target starts, populate
  it using the existing verified file manifest and supervisor-owned blob store,
  then apply and verify ownership and modes. No extra service or installer is
  needed. Do not expose the blob store to application code.
- Record volume creation intent in the existing host-private Docker binding
  before creation. Preserve those records when recording the container ID.
  Verify the pinned daemon, session label, volume name and local driver before
  using or deleting a volume. Reject collisions with existing volumes.
- Session purge MUST remove its retained volumes only after confirming the owned
  target is stopped. Uncertain identity or Docker failure MUST preserve the
  session binding for retry. Purge MUST NOT delete unrelated volumes.
- Document that host runtime directories are staging data; live mutable
  files reside in Docker. Checkpoints remain host-owned portable manifests/blobs,
  and restoring a checkpoint MUST NOT depend on its source volume remaining.

## Verification and documentation

- Unit tests MUST cover logical-owner journal replay, digest protection,
  ownership-only changes, unknown-owner rejection, legacy rejection, content
  verification, root/symlink escapes and capture limits.
- The native Docker qualification MUST use distinct application/supervisor UIDs.
  Capture an empty baseline, create nested 0700/0600 private state through an
  application operation, capture it, reset to the baseline and prove the content
  is absent. Restore private state, read and update it through the application,
  verify exact modes and owners alongside supervisor seed files, and capture
  and restore the updated state again.
- Exercise Linux and macOS/Docker Desktop. Retain qualification results outside
  the source checkout and clean up test resources through ordinary session purge.
  Qualification MUST verify removal of all owned mutable volumes without a
  privileged file-permission workaround.
- Add the regression to the existing downloaded-archive CI qualification and
  document ownership scope, startup behavior and legacy-checkpoint handling in
  snapshot documentation. The exported ownership metadata is consumed by
  Operator through the evidence API and MUST be covered by the integration
  contract, including its formats and native digest rules.
