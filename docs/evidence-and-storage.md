# Evidence and storage

Interceptor uses ordinary local files so sessions can be inspected, verified,
archived, and moved without a database.

There are three main kinds of saved data:

- **events** describe what happened during the test;
- the **state journal** describes changes needed to rebuild a snapshot; and
- the **blob store** holds larger bodies and changed file contents without
  duplicating identical data.

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

`events.jsonl` contains one JSON event per line and separates evidence by
purpose:

| Channel | Examples |
| --- | --- |
| `attack` | completed application turns and attacker-sink observations |
| `control` | session lifecycle, injection administration, snapshots |
| `evaluation` | model exchanges, fixture hits/gaps, stateful-service and MCP tool activity |

Each event records when and where something happened, what kind of event it was,
whether it came from the application, sandbox, or test harness, and any related
turn or injection. Larger raw content may be stored separately and referenced
from the event.

For a model exchange, the evaluation event identifies the application's native
codec and selected host provider. It separately references the final injected
application request, the request accepted by the provider adapter, and the
native response returned to the application. These are usually the same API
shape because the MVP uses like-for-like routing, but each stage has its own
digital fingerprint. Authorization headers, API keys, AWS credentials, and AWS
request signatures are excluded.

Bedrock is called through the native AWS SDK, which owns HTTP serialization and
signing internally. Its provider-specific evidence is therefore a canonical
JSON representation of the SDK Converse input/output rather than a capture of
the signed wire bytes.

The `provenance` field identifies the source of an event—for example, the
application, the sealed environment, the test harness, an attacker sink, or
Interceptor itself. Each event also contains a fingerprint of the previous
event. This linked history reveals deleted, inserted, reordered, or edited
entries. It is an integrity check, not an external signature: a trusted local
operator could still replace the entire evidence set.

## State journal

`state.jsonl` is separate because it answers a different question: not “what did
we observe?” but “which changes must be reapplied to rebuild this state?” It
contains ordered changes to services, injections, saved files, usage limits,
oracle results, and virtual time. Like the event log, its entries are linked so
editing can be detected.

Do not edit either JSONL file. Any modification invalidates the chain and can
make checkpoints unrestorable.

## Blob store

A **blob** is simply a larger piece of content stored in its own file. Large
requests, responses, and changed application files are stored once under a
SHA-256 digital fingerprint. Events and state changes refer to that fingerprint
instead of copying the content into every record. Interceptor checks both the
fingerprint and size when reading it.

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

`logs tail` checks the linked event history before printing the selected existing
events. `logs verify` additionally checks that:

- the state-change history is complete and belongs to the session;
- every referenced large file is present and unchanged;
- each snapshot points to valid positions in the saved histories; and
- the approved application and Environment have not changed.

Run verification before interpreting or exporting evidence and again after
moving it between storage systems.

## Export

A stopped or error session can be exported:

```sh
interceptor logs export <session-id> --output evidence.tar
```

The tar archive contains:

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

The default bridge also creates short-lived request and response files. They are
private to the current user, appear to the receiver only after they are fully
written, and are cleaned when completed, canceled, or expired. A sudden machine
failure may leave some behind until Interceptor's cleanup runs. Use an encrypted
local volume where required and do not put real production secrets in an
Environment Blueprint.
