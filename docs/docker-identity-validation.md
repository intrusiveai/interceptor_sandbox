# Docker identity validation

Validated on 2026-09-18 using macOS 26.6.2 arm64 and a local Docker Desktop daemon.
The integration run used isolated data, reference application images and a mock
model provider; it did not change the administrator's active Docker context.

## Automated checks

- `go test ./...` passed.
- `go vet ./...` passed.
- Race detection passed for `internal/ociruntime`, `internal/filemanagement`,
  `internal/hostrun`, `internal/localapi` and `cmd/interceptor`.
- Regression tests cover create-before-start persistence, changed ambient Docker
  settings, full-ID lookup after rename/name reuse, daemon replacement/unavailability,
  label mismatch, absence without exit proof, interrupted-create reconciliation,
  name-only legacy records, evidence preservation and Unix-transport orphan checks.

## Docker Desktop integration

1. Ran the existing local API smoke client through native attempt/feedback,
   checkpoint creation, restore, replacement invocation, stop and both evidence
   exports. Every session saved a full Docker ID and host exit receipt.
2. Started a separate test session and renamed its live container.
3. Created a stopped test container reusing the original name.
4. Killed the Interceptor host process with SIGKILL, confirmed its original target
   was still running, then set `DOCKER_CONTEXT` and `DOCKER_HOST` to invalid values
   for the independent administrative CLI process.
5. `interceptor stop <session-id>` removed the original target by its saved socket
   and ID, removed its spools, and left the reused-name container untouched.
6. Campaign purge succeeded with those invalid ambient settings and removed the
   selected evidence group and host binding. The test's replacement container was
   then removed explicitly.

Daemon mismatch and failure cases use deterministic CLI doubles; this run did not
restart Docker Desktop. Native Linux integration was not exercised in this pass.
