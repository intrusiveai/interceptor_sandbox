# Managed PostgreSQL and pgvector

A `postgresql` service packages PostgreSQL 18.6 and pgvector 0.8.7 into the
application wrapper. The application uses its ordinary PostgreSQL driver and
pool. Each session starts a private database from the sealed schema and seed SQL;
no host PostgreSQL, database container, or runtime download is required.

See the runnable [Python and Node examples](../examples/postgresql/README.md).
Build the target image, put its inspected digest in the Blueprint, then use the
normal `environment validate`, `environment seal`, `app build`, and `run` commands.
The first wrapper build downloads checksum-pinned upstream sources and compiles
native Linux binaries inside Docker. Subsequent builds can reuse Docker layers.
Linux AMD64/ARM64 and macOS AMD64/ARM64 use the matching Linux runtime; macOS
uses its packaged Linux companion and needs no local Go or database toolchain.

## Blueprint

```yaml
network:
  dns: {enabled: true}
  tls: {enabled: true, trust_mode: auto}
services:
  - id: database
    kind: postgresql
    listen: 127.0.0.1:15432
    hostnames: [database.sandbox.test]
    postgresql:
      database: support
      roles:
        - {name: agent, password: synthetic-password}
      schema: [assets/schema.sql]
      seeds: [assets/seed.sql]
      tables:
        - id: documents
          schema: public
          name: documents
          keys: [id]
          fields: [body, metadata]
          allow_create: true
          max_records: 100
```

`schema` and `seeds` are ordered local SQL asset lists, sealed with the Blueprint.
Schema SQL declares tables, indexes, grants and vector dimensions. Seed SQL inserts
initial records, related rows and precomputed vectors. The `vector` extension is
enabled first. Initialization completes before the application starts. Invalid
SQL, constraints or vector dimensions fail startup. SQL is sent to PostgreSQL;
psql backslash commands and shell scripts are not supported. Grant application
roles only the table/sequence permissions they need; they cannot be superusers,
create roles/databases, replicate, bypass RLS, or hold other roles.

Query and document vectors must use the same embedding representation. Seed
loading does not call an embeddings model or regenerate supplied vectors.

Use synthetic credentials, never production credentials. Declare the password in
`credentials` with `environment_variable: PGPASSWORD`, the same synthetic `value`,
and `accepted_by: database`, as in the runnable examples. The example configures
libpq/Psycopg using `PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`, `PGPASSWORD`,
`PGSSLMODE=verify-full`, and `PGSSLROOTCERT=/opt/interceptor/tls/ca.pem`. Node `pg`
uses its normal `ssl.ca` configuration with that PEM and verification enabled.
Certifi configuration does not configure libpq. Authentication uses native
SCRAM-SHA-256. Channel binding `require` is unsupported because the proxy
terminates TLS; do not configure it. The private backend has no TCP listener.

## Limits and compatibility

| Field | Default | Allowed range |
| --- | --- | --- |
| `max_connections` | 16 | 1–128 |
| `query_timeout_ms` | 30000 | 100–120000 |
| `max_result_bytes` | 8388608 | 1024–67108864 |
| `max_storage_bytes` | 268435456 | 33554432–10737418240 |

Each client keeps its own backend connection. Simple/extended queries, prepared
statements, transactions, savepoints, SQL cursors, portal fetches, typed parameters,
and binary results retain native execution. Sync groups preserve reply ordering;
the proxy waits for a group's completion before forwarding the next group.
COPY and replication are unsupported and fail explicitly. Results are buffered
within `max_result_bytes`; excess closes the exchange with incomplete evidence.
Storage is checked at admission and every 100 ms while running; it is a watchdog,
not an exact filesystem quota, so a running write may briefly overshoot the limit.
Protocol messages are bounded to 1 MiB on ingress. Keep database and snapshot
budgets large enough for PostgreSQL's catalog and WAL overhead.
Each connection permits up to 256 tracked statements, portals, and SQL cursors.
Close unused prepared statements and cursors. Expired protocol portals are
released at transaction end. Cancellation has bounded handshake capacity even
when the application connection pool has reached `max_connections`.

Names use ASCII letters, digits and underscores, start with a letter or
underscore, and contain at most 63 characters. Application roles exclude reserved
`pg_`/`interceptor_` names and `postgres`. Declare 1–16 roles with 8–256 byte
synthetic passwords, at least one schema asset, and at most 128 schema/seed
assets combined. Table policies require 1–8 key fields and 1–64 writable fields;
keys cannot also be writable fields. Creation requires `max_records` from
1 through 100000.

The wrapper records the database versions and builder's supervisor UID in its
existing application lock. Rebuild if running under a different host UID.
Application execution retains its separate restricted UID. Database data,
evidence, TLS keys and the Blueprint are mounted beneath a
supervisor-owned Linux directory; their ordinary paths resolve through fixed
symlinks. This protects them even when Docker Desktop remaps bind-mount ownership.
Management sockets use a separate supervisor-only directory on container tmpfs.
Runtime executables use their
packaged musl library, independent of the target image's C library.

## Injection and committed-state checks

Database response injection uses `surface: database_response`, `mode: simulation`,
`scope: once` or `standing`, and `selector.service`. Existing request-body
predicates match a bounded object containing `query`, `parameters`, and `result`.
Parameters expose `oid`, `format`, and `value`; binary values use base64. `result`
is the zero-based result index within an execution. Call ordinals count matching
nonempty captured results under the existing serialized injection manager.

Placement targets `/rows/<row ordinal>/<column ordinal>` (then JSON keys for
JSON/JSONB). For example, `append_text` at `/rows/0/0` changes the first returned
text cell. Use SQL `ORDER BY` when row order matters. Column ordinals disambiguate
duplicate aliases. Text/varchar and JSON/JSONB can be changed in text or binary
formats; other types pass unchanged. Row/column counts, NULL semantics, type
metadata and native command outcomes cannot be replaced. Invalid placement is
reported as failed and never releases malformed database values.

State injection uses `surface: environment_state`, `mode: fidelity`, and existing
placement operations. `selector.collection` is the declared table policy ID;
`selector.key` is a JSON object containing every declared key column, including
composite keys. Only declared writable fields can change. Record creation requires
`allow_create`, a positive `max_records`, root `replace`, and a complete JSON
record including matching keys. It never silently upserts. Native constraints,
triggers and type conversions apply. Disabling an injection does not undo a
committed mutation. Neither injection mode changes the sealed seed assets.

A `database_query_match` oracle independently runs sealed read-only SQL through
the private management path, bypassing response rewriting:

```yaml
oracles:
  - id: refund-committed
    kind: database_query_match
    service: database
    match:
      query: SELECT count(*)::int FROM refunds WHERE amount=42
      equals: [[1]]
```

Results are arrays of rows. Checks run at initialization and idle transaction
boundaries. A verdict requires a result different from the baseline and equal to
`equals`. Harness mutations reset the baseline, so a harness-created record alone
does not establish an application outcome. Session-wide verdicts are cumulative;
observed turn association is not causal proof. This conservative comparison cannot
assign causality among concurrent application transactions.

To receive database outcomes between attempts, include `oracle-assisted` in the
Blueprint's `adaptive.feedback_profiles` and start the session with
`--feedback-profile oracle-assisted`. Register attempts with that same profile and
include `oracle_outcome` if using an explicit observation selection. After the turn
completes, read its ordinary `observation.read` feedback before starting the next
attempt. No session export or additional database query is needed.

Live feedback uses only recorded application observations bound to that turn and
attempt, through its completion boundary. It does not reuse a cumulative verdict
from an earlier turn or restored checkpoint. A positive result means the condition
was observed, not that this attempt caused a new change. Missing, incomplete or
ambiguous evidence is unavailable/partial, not a negative verdict. A complete
collection without a positive result also does not prove attack failure. SQL,
private results and credentials remain hidden; black-box and diagnostic profiles
withhold oracle outcomes. See [live feedback](feedback.md) for receipt and category
semantics.

## Evidence and restore

Native events retain queries, typed parameters, original results, rewrites,
SQLSTATE errors, command tags and transaction status. Credentials/authentication
exchanges are excluded. Harness mutation records remain distinct from application
traffic. Successful UPDATE inside an open transaction is not proof of commit.
Delivery confirmation still requires the payload in the outbound model request.
Oracles use retained independent query results for live and offline evaluation.

Required capture failures latch `evidence_storage_error`, stop further work and
block new snapshots. Already forwarded writes may have committed; Interceptor
never retries them or asserts rollback without evidence. A failed connection's
unconfirmed transaction outcome remains explicit.

Snapshots drain work and reject open transactions, while allowing idle pools.
They save a native logical database backup, application state, injection progress
and oracle baselines in the existing checkpoint/journal/blob envelope. Backup
bytes count toward snapshot budgets. Restore checks integrity/versions, creates a
fresh backend and restores data without replaying seeds or application writes.
Pools reconnect during normal initialization. Application memory still requires
the existing application state adapter.

## Redistribution notices

Inside database wrappers, `/opt/interceptor/postgresql/licenses/INDEX` lists
PostgreSQL, pgvector, musl and the GCC runtime notices. Full upstream source
archives preserve bundled component notices, including regex and portability
code. GPLv3 plus the GCC Runtime Library Exception accompanies statically linked
GCC runtime code from the native GCC build. Build-tool versions are retained in
`build-packages.txt`. Wrapper qualification checks these files in the produced
image. The CLI archives contain the embedded build recipe and accompanying
license assets; the native database is assembled during `app build`.

Go protocol-client dependency notices accompany the existing `LICENSE` file in
CLI archives and `/usr/local/share/interceptor/licenses/PostgreSQL-client.txt`
in wrappers, including wrappers that do not declare a database.
