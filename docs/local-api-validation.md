# Local integration validation — 2026-09-15

Steps 1–3 of the local MVP integration are complete: aligned contracts,
Interceptor implementation, and an actual container lifecycle test. Operator
runtime implementation has not started. The next checkpoint is the agreed gap
review before Operator implementation.

## Verified behavior

The reference application ran in a network-disabled Docker container on Docker
Desktop (Docker server 29.0.1, Linux aarch64; macOS arm64 host, Go 1.26.5).
The provider was the deterministic localhost mock and the private bridge used
file spool. No external provider credential or call was needed.

The test exercised:

- integrated `interceptor run` startup on `127.0.0.1:8080`, with a competing run
  rejected before target creation;
- campaign attachment, idempotent reattachment and rejection of another campaign;
- native artifact/attempt registration, application invocation and observation;
- snapshot creation, a second state-changing invocation, and restore;
- reference application counter rollback from 2 to 1;
- fresh native session ID and run revision 2;
- exact duplicate restore returning the original replacement identity;
- rejection of a mutation carrying the old session ID;
- another application invocation after restore, followed by confirmed stop;
- complete protected evidence exports for both sessions while the host API stayed
  alive; and
- successful CLI verification of both native event/state journals and checkpoints.

Source session: `sess-1a0a80be05e-c89949705654676352dc6853`. Restored session:
`sess-1a0a80beb08-ab43cc979c4779ad179b1424`. Checkpoint:
`cp-1789525747040-2a7e5fbe204897cb438c3bb2`.

| Evidence | Bytes | SHA-256 |
|---|---:|---|
| sess-1a0a80be05e-c89949705654676352dc6853.tar | 66560 | `883f4c4076f6f7bd028a156c65342480b595c3b808ca149dc911825ba26a0c4d` |
| sess-1a0a80beb08-ab43cc979c4779ad179b1424.tar | 58880 | `8f45bd08c69c45ab14eeb66723524dc5ff6227ade8e7c2f03dd62debf305afdd` |

Native verification found 32 events / 23 state transactions / one checkpoint in
the source, and 21 events / 31 state transactions in the restored session.
The smoke client confirmed both EvidenceBundles report `complete` and match the
campaign/session scope. The original local artifacts are under `/tmp/interceptor-local-e2e-aioq208h`; they are
temporary test data, not a repository dependency.

## Regression and failure checks

`go test ./...` and `go vet ./...` passed. Race-enabled tests passed for local
lifecycle, retained native execution, image construction and sandbox supervision.
They cover restore replay/in-progress status, stopped-source closure,
cross-campaign rejection, stale sessions, stop opt-in, failed stop, invalid
checkpoint preflight, disconnected clients, and terminal close both during
replacement startup and during new-session binding.

The live test exposed a pre-existing wrapper-build issue: copying the public CA
with a file-only `0444` mode also created non-traversable `/opt` parents. The
builder now stages traversable directories and a read-only certificate. The
actual non-root supervisor startup and TLS-backed reference application passed
with that fix. Existing stopped containers from earlier tests were preserved.

## Closure regression validation — 2026-09-16

The integrated API now persists campaign closure after uncertain mutations and
checks authoritative native execution before restore. It also checks the native
close acknowledgement so a fault during preflight cannot be mistaken for planned
controller closure. No separate process or public contract version was added.

Regression tests use the actual native `ExecutionStore` with an in-process target
runtime. They cover 5xx and oversized mutation results, lost native replies,
stale API closure flags, unavailable native status, native closure during
preflight, ledger-write failure, known rejection, and status/cleanup/stop after
closure. The new defect tests fail against the original API implementation and
pass with the fix. Full `go test ./...`, `go vet ./...`, and race-enabled tests for
`internal/localapi`, `internal/adaptive`, `internal/sandboxd`, and
`internal/gatewayhost` passed. This change did not rerun the Docker smoke test;
the source hashes and live results above describe the earlier September 15 run.

## Campaign access regression validation — 2026-09-16

Worker ID, campaign revision and internal service identity no longer gate access
within a campaign. New tests exercise different-worker mutations and reads,
retained-session reporting, native and lifecycle duplicate suppression with changed
attribution, immutable original audit records, changed-command conflicts, new
mutated commands, different-worker closure, stale target rejection and campaign
separation. The restored-injection test replays real source injection journal
entries into a fresh native session/store, then closes and cleans up from later
workers/revisions without the original arm record.

Full `go test ./...`, `go vet ./...`, and targeted race tests for localapi,
adaptive, sandboxd and gatewayhost passed. The standalone smoke client supplies
the new attach/lifecycle attribution fields. The Docker smoke test was not rerun;
the September 15 source hashes and live-container results remain historical.

## Snapshot metadata and discovery validation — 2026-09-16

New unit/API tests cover campaign/description persistence and integrity checking,
creation forwarding with authoritative campaign ID, invalid metadata/body overrides,
CLI create/list/inspect round trips over private Unix transport, campaign-wide
pagination and earlier-session inspection, repeated baseline restore, reads after
stop without restart, campaign/source separation, corrupt/missing inventory,
limits and canceled reads. The service restore tests use a fake target runtime;
retained-file tests use actual checkpoint storage. These do not qualify Operator's
future live-harness restore implementation.

Full `go test ./...`, `go vet ./...`, and race tests for localapi, sandboxd and
snapshot passed; the subsequently added CLI test passed separately. The smoke
client now checks both metadata fields and inventory/inspection before restore,
after restore and after stop. Docker smoke was not rerun for this change; the
September 15 image/source identities and container results above remain historical.

## Live feedback validation — 2026-09-16

The feedback update adds immutable per-invocation v1alpha2 views, observation
selection and receipt-scoped content reads. Regression tests exercise black-box,
diagnostic and oracle-assisted filtering; fixed receipts despite later events;
chunk assembly/digests; empty versus unavailable output; failed invocation status;
explicit truncation and entry limits; missing/corrupt content; invalid ranges;
ambiguous turn selection; source/attempt/campaign separation; and retained reads
after stop without target restart. These native tests use in-process target
fixtures and real receipt/blob/journal files; they do not run an Operator harness.

Full `go test ./...`, `go vet ./...`, and race tests for adaptive, sandboxd,
gatewayhost and localapi passed. Subsequent feedback-boundary/failure/limit
adjustments passed the affected race-enabled tests. The capability fixture was
regenerated for the advertised feedback contract and content-read operation.
The smoke client now selects the exact invocation and reads a feedback chunk.
Docker smoke was not rerun; earlier image/runtime qualification remains historical.
Shared Operator schemas were parsed and their references checked through the
offline catalog; generated SDK/schema semantic conformance remains pending.

## Reproducibility and limits

The standalone [smoke client](../examples/local-api-smoke/main.go),
[setup guide](getting-started.md) and [API contract](local-api.md) describe the
reproduction path. Machine-readable results and hashes of all Go sources plus
module files are in [the validation record](test-results/local-api-2026-09-15.json).
The combined code-manifest SHA-256 is `6f511ac3060eeadbb363862073a5f643838cf83f3a2b619ecc6b365208fce19c`. These identify the tested
working-tree bytes, including previously uncommitted changes; they do not claim a
clean commit or released production source pin.

This qualifies the reference workload over Docker Desktop file spool. It does
not qualify every target/provider, the native Linux Unix-socket deployment, or
Operator's future container launcher, sensors, accounting, journals and client.
Remote transport, authentication, multiple simultaneous instances and automatic
service recovery are deliberately outside the accepted MVP scope. Rebuild wrapper
images when deploying the updated supervisor/builder; old images do not acquire
these changes automatically.

## Terminal execution status (2026-09-18)

The host closes API execution when native metadata records an error, the target
exits unexpectedly, or the model connector fails. The instance ledger and status
response retain the first failure's session ID, reason and UTC observation time.

Validation for this change:

- Full Go tests and `go vet ./...` passed.
- Race-checked host/runtime and API tests cover native limits, unexpected exits,
  connector failures, clean planned stop, native error during planned stop,
  incomplete stop finalization, and cleanup/ledger failures.
- Restore tests cover rejected preflight racing with failure, replacement failure
  before binding publication, and delayed callbacks from a previous target.
- A blocked fault callback does not prevent the independent Docker kill. Failure
  notification also bypasses the API mutation gate. Later successful cleanup does
  not erase the original failure state.
- On macOS 26.6.2 arm64 with local Docker Desktop, the reference application's
  native `WALL_TIME_LIMIT` autonomously produced `phase: error`, `closed: true` and
  the exact reason in `/v1/status` and the persisted ledger. Only status polling
  followed attachment; no failed experiment request was needed to update status.
  The API stayed available, spools were removed, and evidence export succeeded.
- The real-Docker healthy restore/attempt/stop/export smoke test also passed;
  planned completion retained `phase: stopped` with no failure object.

These checks exercise Interceptor's host reporting path; they do not qualify an
Operator launcher or constitute a native Linux integration run.

## Snapshot caller byte allowance (2026-09-18)

The typed `snapshot.create` request now carries optional
`maximum_committed_bytes`. The supervisor checks it against current native
remaining bytes under the snapshot lock and rejects oversized candidates before
checkpoint commitment. Operator's campaign defaults and accounting rules are
specified separately; this change does not implement Operator's future ledger.

Validation for this change:

- `go test ./...` and `go vet ./...` passed.
- Race-enabled tests passed for `internal/sandboxd`, `internal/snapshot` and
  `cmd/interceptor`.
- Real gateway/supervisor tests over Unix sockets and file spool create snapshots
  from a temporary filesystem with an independently paused child process. They
  cover zero, exact boundary, caller/native allowances in both orders, omitted
  allowance, and continued caller enforcement after simulated native-counter
  rollback. Rejected candidates leave no checkpoint or snapshot counter charge.
- Identical request replay returns the original checkpoint without a second
  charge; changing only its allowance conflicts. CLI transport tests preserve
  omission versus explicit zero and reject negative input before dispatch.
- Parser/arithmetic tests cover null, invalid numeric types/ranges and saturating
  native-limit arithmetic.

These are host-side tests of the real handlers/transports; this change did not
rerun Docker qualification or exercise an actual restore in its new quota test.
Updated supervisor images must be rebuilt to expose and enforce the new field.

## Explicit injection cleanup support (2026-09-18)

Operator's new `engine.injection_delete` contract addresses a known attempt
receipt/action in the current campaign target. Interceptor already permits
campaign-scoped deletion of restored injections without the original arm record.
Its handler now distinguishes confirmed absence (`404 not_found`) from journal
failure (`500 injection_delete_failed`), enabling safe host normalization of
already-absent cleanup.

- Full `go test ./...` and `go vet ./...` passed.
- Race-enabled `internal/sandboxd` and `internal/injection` tests passed.
- The restored-state test verifies cross-campaign rejection, deletion under a
  different worker/revision, and repeated deletion returning confirmed absence.
- A real deletion-handler test with a closed journal verifies that storage failure
  returns 500, retains the injection and does not advance the native revision.
- Operator's 28 structural cleanup fixtures and all eight schema definitions
  validate. These checks do not implement its pending handle-resolution client or
  qualify a running Attack Harness cleanup tool. Shared semantic conformance remains a gate
  before Operator/Attack Harness runtime integration.

Rebuild supervisor images when deploying the deletion error distinction.
