# Host lifetime validation

Validated 2026-09-17 on macOS 26.6.2 (25G83), arm64, with Docker Desktop's
Linux arm64 daemon. This qualifies the exercised Interceptor behavior; it does
not qualify Operator's future launcher or every supported host tuple.

## Automated checks

- `go test ./...`: passed. The bootstrap test's startup deadline was increased
  to tolerate concurrent package builds; production deadlines are unchanged.
- `go test -race ./internal/hostlifetime ./internal/hostrun ./internal/localapi ./internal/ociruntime`:
  passed. Tests cover reference-counted assertion lifetime through restore, permanent
  closure after changed sleep markers, lost helpers/unreadable state, API checks
  before restore admission, and Docker kill/spool cleanup during blocked bootstrap.
  A disconnected fake Docker event stream follows the same terminal path.
- `INTERCEPTOR_TEST_REAL_POWER=1 go test -race ./internal/hostlifetime`: passed on
  the real macOS host. Read kernel sleep/wake timestamps, confirmed the assertion
  owned by the actual caffeinate PID, checked its separate terminal signal group,
  and verified assertion removal after release. This opt-in test does not force
  sleep or change persistent power settings.
- `go vet ./...`: passed.
- `CGO_ENABLED=0` CLI builds for darwin/amd64, darwin/arm64, linux/amd64 and
  linux/arm64: passed. Cross-compilation is not runtime qualification.

## Real Docker checks

Used isolated test data, a locally built reference target/supervisor image, and the
mock provider. The existing `examples/local-api-smoke` client completed native
attempts, snapshot creation/restoration, post-restore invocation, target stop and
verified evidence export for both sessions.

Additional host checks passed:

1. The actual caffeinate PID owned an idle-system-sleep assertion before API use.
2. The same helper PID remained across the healthy restore; no replacement helper
   appeared. Target stop released it while the host API still served evidence.
3. Killing only the test's caffeinate helper closed API execution, killed/removed
   the test target, deleted its spools and retained `SLEEP_PREVENTION_LOST`.
4. Sending SIGTERM to the test host process terminated its target, removed its
   spools, released the assertion and retained `HOST_SERVICE_INTERRUPTED`.
5. Exporting that interrupted session included `host-interruption.json`.

Physical sleep/lid close, actual logout, Docker Desktop restart and installed
LaunchAgent behavior were not forced on this workstation. Sleep-marker changes
and Docker disconnects were simulated; SIGTERM exercised the logout signal path.
Those disruptive deployment checks remain distinct from the passing tests above.
