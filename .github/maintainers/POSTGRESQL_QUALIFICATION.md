# PostgreSQL integration qualification

Qualification date: 2026-10-28. Implementation revision:
`c6c3da449a954777cfd8a757a4910d6c007a5370`.
The subsequent qualification-report commit changes documentation only.

The implementation is on `feat/postgresql-integration`, intended for
`candidate-v0.1.3`. No branch was pushed, workflow dispatched, or release published
during implementation. The specification is [postgresql-integration.md](../../specs/postgresql-integration.md).

## Actual packaged workflow results

All three runs used the same extracted macOS ARM64 release archive outside the
source checkout, with Go absent from the invoked CLI's PATH. Docker ran native
Linux ARM64 wrappers containing the archive's verified Linux companion. Native
PostgreSQL 18.6 and pgvector 0.8.7 were built by `interceptor app build`; execution
required no host PostgreSQL or runtime downloads.

| Qualification | Result | Runtime and clients |
| --- | --- | --- |
| Python example | Passed | Python 3.12.16, Psycopg 3.2.10, pgvector Python 0.4.1 |
| Node example | Passed | Node 22.23.4, `pg` 8.16.3 |
| Unchanged customer-support agent | Passed | Python 3.12.16, Psycopg 3.3.6, pool 3.3.3, pgvector Python 0.4.2, LangChain 1.4.7 |

The maintained examples verified populated relational/vector seeds and joins,
TLS-verified pooled connections, response-only injection, persistent update and
allowed record creation, duplicate-creation rejection, committed versus
rolled-back refunds, and coordinated database/application/injection restore.
Retained native evidence confirmed model delivery for both injection modes,
negative rollback oracles, and immutable request observation on results.

Each example also induced a required blob-storage failure and verified a failed
turn, blocked subsequent work/snapshots, and an inspectable partial export. The
Python run retained 1025 events; Node retained 484. Each performed one restore
and produced one intentionally partial session. Their event integrity checks
passed.

The customer-support test exercised its original schema/version checks, pool,
pgvector lookup, document validation, customer/order tools, and refund transaction.
An independent database oracle verified a committed refund of 1250 cents. The
agent's existing model/embedder hooks supplied scripted decisions and fixed
384-dimensional test vectors. This test did not call a live provider or depend on
agent-authored evidence. Agent source digests were retained by the runner.

Application-identity probes could not open the private Blueprint or TLS key,
enter database data, or traverse the native socket directory. These checks caught
Docker Desktop's bind-mount ownership remapping during development; the final
wrapper uses a supervisor-owned native Linux ancestor for private mounts.

The native protocol test also passed inside the final Python wrapper. It exercised
full-pool cancellation, invalid credentials/hostname/management-role rejection,
independent transactions, savepoint rollback, error recovery, multiple SQL
results, unions/grouping, text/binary JSONB rewrites, NULL and unselected values,
260 successive transactions with partial portal fetches, fresh-session seed
isolation, and actual backup/restore. It used pgx 5.11.0 against the packaged engine.

## Archive and repository checks

All four archives passed the existing integrity verifier. Each Darwin companion
matched the corresponding Linux executable byte-for-byte. The macOS ARM64 archive
was rebuilt twice with identical archive/checksum bytes using Go 1.26.5.

| Archive suffix | SHA-256 |
| --- | --- |
| `linux_amd64.tar.gz` | `8201e4f1df5601107ca9e83626b006ccbbc322a508addbd8a21cf03516f36130` |
| `linux_arm64.tar.gz` | `90ebae32610567e2a647d9f602bb8a57ea67c9cf8e8a5e1db7ee084a3edbe478` |
| `darwin_amd64.tar.gz` | `d599671c03dc6989e2b8b8ba299ed99eeb2332532f50981b483670767d1be286` |
| `darwin_arm64.tar.gz` | `f9864746a01599aed3082ca474931e5b6d8f2a2113429d39f505625f1341caa9` |

Every filename has the prefix `interceptor_sandbox-v0.1.3-`.
The exercised Linux companion digest was
`1f6aa25b39893046448715e65fac41a3046d38a83d5cc948690a2280df32795d`.

License qualification inspected the actual final images and CLI archive. It
verified complete PostgreSQL/pgvector source-and-notice archives against pinned
digests, their main notices against upstream archive contents, musl/GCC runtime
notices, and the compiled PostgreSQL client dependency notices.

`make test`, `go vet ./...`, and race tests for PostgreSQL, sandboxd, injection,
and hostrun passed. The release-publisher tests/race/vet checks are included in
`make test`. Workflow YAML and run/uses step shapes were validated locally.

## Reproduction and validation boundaries

Use the [qualification commands](../../examples/postgresql/README.md), including
`--database-test`, with an archive matching the native host. Local result bundles
from this session are `/tmp/interceptor-pg-qa-{python,node,agent}-c6c3da4/`; archives
are under `/tmp/interceptor-pg-release-c6c3da4-<os>-<arch>/`. These temporary files
are not permanent release evidence. Test containers, sessions and named test
images were removed; the results were retained for review.

GitHub Actions was not run. The updated workflow downloads its own CI-produced
archives and qualifies Python/Node plus the native protocol suite on Linux AMD64
and ARM64 before candidate publication. Existing publication/promotion conditions
are unchanged. Those checks must pass before publication.

AMD64 runtime execution and native Linux-host wrapper construction were not
performed locally. Local execution covered macOS ARM64 and its Linux ARM64 Docker
companion. No live embeddings/model service was used. The controlled storage-fault
test does not constitute exhaustive crash testing at every COMMIT/evidence-write
boundary; no cross-store atomicity is claimed.

COPY, replication, and required SCRAM channel binding are explicitly unsupported.
Response mutation supports text/varchar and JSON/JSONB values; other native types,
including vectors, pass through unchanged. See the [database guide](../../docs/postgresql.md)
for limits and configuration.
