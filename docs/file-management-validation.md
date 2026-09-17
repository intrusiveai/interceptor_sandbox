# File management validation — 2026-09-17

## Implemented behavior

- `run --spool-max-bytes` overrides `INTERCEPTOR_SPOOL_MAX_BYTES`; the default
  is 536870912 bytes. The effective value is stored in session metadata and
  preserved by restore.
- Independent one-second host checks count both spools, including temporary and
  unexpected nested files. Excess or inspection failure closes execution and
  triggers Docker termination. The periodic check permits overshoot.
- Confirmed container exit and stopped host writers permit complete spool removal.
  Failed cleanup retains evidence and records pending cleanup. Startup retries
  abandoned spool cleanup without replaying messages.
- Session-local blobs, managed exports and copied restore provenance make retained
  replacement evidence independent of its source session.
- `purge --session`, `--campaign`, and `--all` remove complete groups; `delete`
  aliases session purge. Purge refuses active/unconfirmed containers and maintenance
  lock conflicts. The local API service must exit before purge in this MVP.

## Automated checks

`go test ./...`, `go vet ./...` and targeted race-enabled tests passed.
Host binaries built for macOS arm64/x86_64 and Linux arm64/x86_64.
The regression suite covers existing lifecycle, feedback, snapshot and command
behavior. New tests cover aggregate size accounting, exact quota boundaries,
configuration precedence, temporary files, invalid links, cancellation, growth,
unresponsive bootstrap termination, preserved evidence, stopped-writer cleanup,
full-selection purge preflight, lifecycle inventory pruning, external-copy
preservation, independent blob copies and closure without waiting for a mutation
handler. Fault records are bounded metadata outside the spool.

## Docker integration

An isolated reference application and rebuilt supervisor ran on Docker Desktop
29.0.1, Linux arm64 containers on macOS arm64, with a deterministic localhost mock
provider and file-spool transport. The test used private temporary data and separate
image tags, without external model calls.

Verified:

1. Campaign attachment, invocation, checkpoint metadata and inventory, restore,
   counter rollback, fresh target binding and duplicate-restore suppression.
2. Default quota and campaign ID persistence in source and replacement metadata;
   session-local blob stores with no shared blob directory.
3. Complete spool removal after restore/stop, with journals and artifacts retained.
4. Purge refusal while the local API service was still running.
5. Successful source-session purge, followed by verification and export of the
   retained replacement without its parent directory.
6. Campaign purge, all-data purge, and preservation of administrator exports.
7. A custom 1 MiB quota exceeded by a 1 MiB + 1 byte temporary spool file: API
   closure, Docker termination, `SPOOL_SIZE_LIMIT` evidence, and complete spool cleanup.

The byte-overflow test wrote the temporary file from the host into the actual
container's spool directory. It verifies the host watcher and Docker termination
path; it is not a filesystem capacity guarantee. Native Linux runtime behavior is
covered by portable Go code/tests, not a live Linux-host qualification in this run.

Deployment requires the updated host binary and rebuilt supervisor image together.
New sessions use the session-local evidence layout. Existing development sessions
created with a shared blob directory are not automatically migrated; retain their
matching older tooling for export before retiring that data.
