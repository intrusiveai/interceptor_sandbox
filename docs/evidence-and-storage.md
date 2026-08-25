# Evidence and storage

Interceptor uses ordinary local files so sessions can be inspected, verified,
archived, and moved without a database.

## Data root

By default, data is stored below the operating system's user configuration
directory in an `interceptor` subdirectory. Set an explicit location when
automation needs a predictable path:

```sh
export INTERCEPTOR_DATA_DIR=/path/to/interceptor-data
```

The layout is conceptually:

```text
<data-root>/
  applications/                 wrapper-image locks
  blobs/sha256/                 shared content-addressed objects
  sessions/<session-id>/
    session.json                lifecycle, digests, limits, URLs, ancestry
    events.jsonl                hash-chained observations
    state.jsonl                 hash-chained logical transactions
    file-manifest.json          latest mutable-root manifest
    checkpoints/*.json          logical restore points
    generations/                session-specific writable roots
    logs/
      application.stdout.log
      application.stderr.log
    sockets/                    protected spool or Unix bridge artifacts
```

Treat the entire root as sensitive test data. Configure it on a local filesystem
owned by the operator; do not place it in a broadly shared or synchronized
directory.

## Event channels

`events.jsonl` separates evidence by audience:

| Channel | Examples |
| --- | --- |
| `attack` | completed application turns and attacker-sink observations |
| `control` | session lifecycle, injection administration, snapshots |
| `evaluation` | model exchanges, fixture hits/gaps, stateful-service activity |

Each event records a schema version, session and sequence, timestamp, channel,
layer, kind, provenance, semantic JSON, optional turn/injection attribution,
optional raw blob reference, previous hash, and hash.

Provenance values distinguish application behavior, sealed environment behavior,
harness injection, attacker infrastructure, and sandbox control. Hash chaining
detects deletion, insertion, reordering, or modification; it does not prevent a
local trusted operator from replacing an entire evidence set and is not a
third-party timestamp or signature.

## State journal

`state.jsonl` is separate because it is an executable restore record rather than
an observation stream. Each synced transaction has ordered operations, a reason,
sequence, previous hash, and hash. It includes service mutations, injection
state, filesystem delta operations, budgets, oracle state, clock state, and
restore ancestry.

Do not edit either JSONL file. Any modification invalidates the chain and can
make checkpoints unrestorable.

## Blob store

Large request/response bodies and changed regular files are stored once by
SHA-256 digest. Logs and journal operations use `sha256:...` references. Blob
reads verify the expected digest and size.

Blobs are shared across sessions and retained by `interceptor delete`. The MVP
does not provide garbage collection or secure deletion. If retention policy
requires erasure, stop all sessions, inventory all required evidence, and manage
the dedicated data root as a whole using approved host procedures.

## View and verify

```sh
interceptor logs tail <session-id> --channel attack
interceptor logs tail <session-id> --channel control
interceptor logs tail <session-id> --channel evaluation
interceptor logs verify <session-id>
```

`logs tail` verifies the event chain before printing the selected existing
events. `logs verify` additionally checks:

- journal chain and session identity;
- every blob referenced by an event or file operation;
- checkpoint identity, digest, and log positions; and
- the current Sealed Environment and application digest binding.

Run verification before interpreting or exporting evidence and again after
moving it between storage systems.

## Export

A stopped or error session can be exported:

```sh
interceptor logs export <session-id> --output evidence.tar
```

The deterministic tar contains:

- an export `manifest.json`;
- `session.json`;
- `events.jsonl`;
- `state.jsonl`;
- `file-manifest.json`;
- checkpoint manifests; and
- all blobs referenced by the included events and journal.

Application stdout/stderr and transient bridge artifacts are not included.
Export first verifies the core chains and bindings. The archive is not encrypted
or signed; protect it with normal evidence-handling controls and add an external
signature if chain-of-custody requirements demand one.

## Sensitive content

Model prompts and responses, application output, synthetic credentials,
canaries, attacker observations, and injected payloads may be persisted in logs
or blobs. The host provider credential is specifically excluded, but this does
not make the remaining evidence non-sensitive.

The default bridge also creates transient request/response files. They use
private permissions, atomic publication, cancellation, expiry, and cleanup, but
may exist briefly on disk and can survive an abrupt host failure until orphan
cleanup. Use an encrypted local volume where required and do not put real
production secrets in an Environment Blueprint.

