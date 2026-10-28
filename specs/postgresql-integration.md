# Managed PostgreSQL, native evidence, and database injections

## Outcome and scope

Interceptor MUST let an unchanged application use PostgreSQL and pgvector inside
its sandbox through its ordinary database driver, including TLS and connection
pooling. Users MUST declare the database in the Environment Blueprint; they MUST
NOT need to install, administer, or connect to a separately managed database.

Interceptor MUST support both:

- **Database-state injection:** transactionally change an eligible stored record
  or create a record where explicitly allowed. The payload persists in database
  state and is subsequently retrieved through ordinary application queries.
- **Database-response injection:** change eligible values in a query result after
  execution and before the application receives them, without changing the
  stored records.

The implementation MUST use packaged native PostgreSQL with pgvector for SQL
execution and an Interceptor-owned PostgreSQL protocol proxy for observation
and response injection. Here, embedded means automatically packaged and managed
within the sandbox lifecycle; it does not mean an in-process database library.
It MUST NOT depend on pgserver or PGlite, reimplement a SQL execution engine, or
translate PostgreSQL SQL into another dialect.

The shared database integration MUST cover configuration, lifecycle, evidence,
oracles, injections, and snapshot coordination. PostgreSQL-specific protocol,
types, and backup behavior MUST remain within its implementation. Small internal
interfaces SHOULD permit later database implementations without introducing a
dynamic plugin loader or implementing MySQL, SQL Server, or Oracle in this change.

## Existing components and contracts

The implementation MUST inspect and extend these existing owners:

- `internal/environment`: strict Blueprint loading, validation, sealed assets,
  and delivery descriptions.
- `internal/appimage`, `internal/ociruntime`, and `internal/sandboxd`: wrapper
  packaging, process identities, closed networking, application startup,
  observation windows, lifecycle, and snapshot coordination.
- `internal/services`: managed service registration and response processing.
  Database-specific code SHOULD live in a focused package under this ownership.
- `internal/injection`: definitions, selectors, placement, attempt identity,
  ordinal/scope state, and delivery confirmation.
- `internal/recording`, `internal/blobstore`, `internal/statejournal`, and
  `internal/snapshot`: integrity-covered evidence and restorable state.
- `internal/evaluation`, adaptive capabilities/feedback, local APIs, and export:
  oracle evaluation, discoverable injection targets, and retained inspection.

The implementation MUST reuse existing evidence, injection, and snapshot formats
with explicit additive extensions. It MUST NOT create another evidence service,
parallel manifest system, or application-authored logging dependency. It MUST
update affected authoritative contracts alongside code, including
`specs/mvp.md`, `specs/delivery-contracts.md`, and
`specs/external-adaptive-simulation.md` where their existing definitions change.
The observed-window and failure policies in `specs/observed-turn-evidence.md`
and application-state coordination in `specs/application-state-snapshots.md`
MUST continue to apply.

## Blueprint and packaging

1. The Blueprint MUST support a managed PostgreSQL service with a stable service
   ID, declared hostname and port, database name, synthetic application roles and
   credentials, ordered schema/seed assets, injection policy, and bounded
   resources. Unknown fields, unsafe asset paths, conflicting listeners, and
   unsupported combinations MUST fail validation before launch.
2. Configuration MUST use the existing versioned Blueprint/service conventions.
   The implementation MUST document exact field names, defaults, ceilings,
   validation, and capability representation and provide a runnable example.
   Existing Blueprints without a database MUST retain their behavior.
3. Schema and seeds MUST be sealed, immutable local assets. Each fresh session
   MUST initialize isolated writable database state from those assets, enabling
   pgvector before dependent schema initialization. Restore MUST load checkpoint
   state without applying seeds again over the restored data.
4. Schema/seed SQL MUST execute as database input, not shell commands or psql
   meta-commands. Initialization errors MUST prevent application readiness and
   report the failing asset without exposing credentials. Loading MUST be
   deterministic in order, with engine versions and asset digests recorded.
5. Wrapper builds MUST package a pinned, maintained PostgreSQL build, compatible
   pgvector, runtime libraries, and required backup/restore tools for Linux
   AMD64 and ARM64. They MUST record artifact versions and integrity using
   existing image/build metadata. PostgreSQL and pgvector updates MUST be
   repeatable; the selected pgvector MUST satisfy the target's required features
   and the qualification agent's minimum version, currently 0.7.
6. Dependency acquisition MAY occur during an explicitly network-enabled build
   using verified, pinned inputs. Sandbox execution MUST require no downloads,
   package installation, compiler, target source checkout, or host PostgreSQL.
   Database packaging MUST work with the existing macOS CLI/Linux companion
   release layout and Linux CLI without adding host-native database requirements.
7. Builds MUST validate the target image's architecture/runtime compatibility
   and fail actionably where unsupported. Existing release integrity and build
   qualification mechanisms MUST cover any new distributed runtime artifacts.
8. Limits MUST bound connections, query/lock/idle-transaction waits, protocol
   messages, result capture, storage, and snapshot usage. Limit exhaustion MUST
   be explicit; it MUST NOT produce a truncated successful query or silently
   incomplete evidence. Reuse existing limits where applicable.

### Schema and populated seed data

Seed support MUST include populated tables, not only schema creation. A Blueprint
MUST be able to reference ordered SQL assets that create schemas/tables,
constraints, and indexes and insert initial records, including related records
across multiple tables. Users MUST NOT need a separate manual import step after
starting the sandbox. SQL assets are sufficient for this requirement; additional
CSV, JSON, or database-dump import formats are not required.

pgvector seeds MUST support rows containing supplied embedding vectors alongside
their document text, identifiers, and metadata. Vector columns and dimensionality
MUST be defined by the schema, and PostgreSQL/pgvector MUST validate seeded values
against those definitions and other constraints. Seed loading MUST NOT call an
embedding provider or regenerate supplied vectors. Any preparation of embeddings
occurs before sealing; the example MUST explain that query embeddings must use
the same embedding model/representation as the seeded document vectors.

All required seed loading MUST finish successfully before application readiness.
Invalid SQL, vector dimensions, or relational constraints MUST fail startup
without exposing a partially initialized database to the application. Fresh
sessions MUST independently load the declared starting records; application
writes and injections in one session MUST NOT alter seeds or another session.
Restored sessions MUST use checkpoint contents, including subsequent record
changes and deletions, without reintroducing original seed rows.

### License compliance for distributed components

The implementation MUST satisfy the redistribution obligations of the exact
PostgreSQL, pgvector, tools, and runtime libraries it packages. It MUST inspect
the license/copyright files supplied with the pinned versions, including bundled
dependencies, rather than assume every component uses PostgreSQL's license.
The upstream [PostgreSQL license](https://www.postgresql.org/about/licence/)
and [pgvector license](https://github.com/pgvector/pgvector/blob/master/LICENSE)
provide references; the selected artifacts' own notices govern packaging.

Every distributed archive or container image containing these components MUST
include their required copyright notices, license texts, and disclaimers in a
documented, locally readable location. PostgreSQL's required notice, permission
text, and warranty/liability disclaimers MUST be preserved in full. A web link or
Interceptor's own license alone MUST NOT substitute for these notices. Any
additional attribution, source-distribution, or other obligations imposed by a
bundled dependency MUST also be fulfilled where applicable.

The build MUST retain the required notices through extraction, image assembly,
and stripping of unnecessary files. It SHOULD reuse an existing third-party
notices convention; otherwise, a simple license directory with a component,
version, and license-file index is sufficient. Dependency updates MUST refresh
the corresponding notices and obligation review.

Packaging qualification MUST inspect the actual produced archives and final
container images for each supported architecture and verify that the required
notices match the packaged versions. Missing or incomplete notices MUST fail
qualification. User documentation MUST explain where recipients can find them.

## Lifecycle, isolation, and TLS

The application MUST connect only to the declared proxy endpoint. Native
PostgreSQL MUST run under an appropriately restricted identity with private data
and socket permissions. A private Unix socket SHOULD connect the proxy and
management code to PostgreSQL. A loopback TCP port alone MUST NOT be treated as
private when the application shares its network namespace.

The target application MUST NOT be able to connect directly to the backend,
read or modify database files, obtain management credentials/private TLS keys,
or impersonate an oracle or administrative connection. Application roles MUST
NOT have superuser, role-management, server-file/program execution, or arbitrary
extension-installation privileges. Schema ownership and grants MUST preserve
the declared application permissions, including row-level security when used.
Database facilities MUST NOT open external network paths or bypass sandbox
filesystem restrictions. No host database directories or credentials may be used.

The implementation SHOULD reuse the existing container and supervisor identity
boundaries. Any additional process isolation needed to meet these requirements
MUST be justified by a concrete bypass test; an external database service or
orchestrator MUST NOT be introduced merely for convenience.

Startup MUST initialize and verify the backend, schema, extensions, roles, proxy,
and required evidence sinks before starting the application. Backend/proxy loss
MUST become a visible service/session failure, not an automatic empty database
reinitialization or query replay. Stop, failed startup, restore, and cleanup MUST
reap owned processes and release their resources without touching other sessions.

The proxy MUST support PostgreSQL TLS negotiation using the existing sandbox CA
and certificates for the declared hostname. Hostname/certificate verification
MUST remain enabled in qualification. PostgreSQL clients' CA configuration
(for example libpq's `sslrootcert`/`PGSSLROOTCERT`) MUST be supported and documented;
Certifi injection alone MUST NOT be assumed to configure a PostgreSQL driver.
No target source edits or disabled verification may be required.

The endpoint MUST validate configured synthetic credentials, with a documented
SCRAM-SHA-256 authentication baseline. TLS/channel-binding behavior MUST be
explicit and tested; the proxy MUST NOT blindly relay a channel-bound
authentication exchange across different TLS sessions. Unsupported authentication
modes MUST fail clearly rather than silently weakening authentication.

## PostgreSQL passthrough and compatibility

Each frontend connection MUST map to an independent backend connection for its
lifetime. The proxy MUST NOT serialize all clients onto one database session or
implement another connection pool. Application pools MUST be able to maintain
multiple idle connections and execute independent concurrent transactions.

The proxy MUST preserve PostgreSQL execution and session behavior for:

- Startup parameters, authentication, connection reuse, and orderly/disordered
  disconnects, including rollback of an open transaction on disconnect.
- Simple and extended queries; Parse/Bind/Describe/Execute/Sync/Close; named and
  unnamed prepared statements and portals; partial portal fetches; and pipelined
  messages with correct response ordering and error recovery.
- SQL-level named cursors (DECLARE/FETCH/CLOSE), including Psycopg server-side
  cursors used by the qualification agent. Cursor fetch results MUST retain their
  originating query context for injection matching while their observation
  association belongs to the execution that actually fetched those results.
- Typed parameters and text/binary results, NULLs, column metadata, row counts,
  command completion, SQLSTATE errors, notices, and transaction status.
- BEGIN/COMMIT/ROLLBACK, autocommit, savepoints, session settings, independent
  connections, row locks, and cancellation routed only to the owning backend.
- CRUD, predicates, joins, UNION/UNION ALL, sorting, grouping/aggregates, row
  limits, common PostgreSQL casts/types, constraints, generated identifiers,
  catalog inspection, and pgvector registration and similarity queries.

Existing maintained protocol codecs SHOULD be reused instead of hand-writing
message framing. The implementation MUST track query/parameter/result association
per connection and execution, including prepared statements and portals. It MUST
NOT reconstruct SQL by interpolating bound parameters or guess which request a
response belongs to. A returned column alias MUST NOT be mistaken for proof of
its originating table, especially for joins and computed expressions.

Supported protocol features, authentication modes, and injection-capable types
MUST be documented separately. An opaque value MAY pass through unchanged only
when its bytes can still be captured under the evidence contract. Unsupported
flows such as unimplemented COPY or replication MUST fail explicitly with a
coverage gap before execution, not bypass recording. Unsupported field rewriting
MUST report a placement failure rather than corrupt a result or claim delivery.
PostgreSQL errors from supported execution MUST remain database errors, not be
misclassified as missing emulator coverage.

## Native database evidence

The proxy MUST record bounded query text, bound parameters with types, result
metadata and values, SQLSTATE errors, and transaction outcomes in the existing
event/blob system. Credentials and authentication secrets MUST NOT be recorded.
Sensitive result content MUST follow existing evidence access/visibility rules.

Each execution MUST have stable connection/execution identifiers and transaction
context. Request observation MUST be captured when the executable request enters
the proxy, before queueing/forwarding, rather than when a pooled connection or
prepared statement was first created. All derived response events MUST retain
that immutable observed-window association. Ambiguous/outside-turn requests and
late results MUST follow existing rules; no application correlation headers or
private agent evidence are required.

Evidence MUST distinguish requested SQL, returned rows/affected-row counts,
successful or failed statements, and committed, rolled-back, or uncertain
transactions. An UPDATE completion inside an open transaction MUST NOT establish
a committed business outcome. A transaction spanning turns MUST NOT be attributed
causally to its last turn merely because COMMIT was observed there.

Injected exchanges MUST preserve original and modified values/digests, placement,
injection identity, origin attempt, and payload digest separately from observed
attempt context. Administrative seed/state-injection/oracle/backup activity MUST
be identified separately from application traffic and MUST bypass response
rewrites without escaping required native evidence handling.

Required request evidence MUST be persisted before forwarding execution. Required
result and rewrite evidence MUST be persisted before the corresponding result is
released. Bounded streaming MAY be used, but failure after partial transmission
MUST terminate the exchange and mark it incomplete; it MUST NOT fabricate normal
completion. Result limits MUST NOT silently drop rows.

Any required blob, event, flush, or journal failure MUST latch the existing
`evidence_storage_error`/incomplete-session policy, stop further database execution
and turn/snapshot admission, and preserve inspection and cleanup. A query or
COMMIT already forwarded MAY have taken effect. The implementation MUST NOT
retry it, assert rollback without confirmation, or report missing evidence as a
negative oracle result. It MUST NOT promise atomicity between PostgreSQL commit
and the separate evidence store; uncertainty at that boundary MUST be explicit.

## Database-state injection

State injection MUST extend the existing `environment_state` / `fidelity`
mechanism rather than create an unrelated mutation API. Sealed policy MUST map
public target identifiers to eligible schema/table, unique key columns, writable
fields, and explicit record-creation permission. Composite keys MUST be supported.
Adaptive callers MUST receive only the existing authorized capability view,
without management credentials, private paths, or hidden oracle definitions.

The implementation MUST use quoted, policy-resolved identifiers and parameterized
values, not arbitrary caller SQL. Existing placement operations SHOULD be reused
where type-correct. The first implementation MUST support replacing, prepending,
and appending eligible text and modifying declared JSON/JSONB fields. Creation
MUST accept a validated complete record within the allowed field contract and
resource limits; it MUST NOT synthesize rows to repair a failed field placement.

An update MUST resolve exactly one record by its declared unique key. Missing or
ambiguous targets, type errors, constraint violations, and lock timeouts MUST
fail explicitly and atomically. Creation MUST be opt-in and must not silently
upsert an existing record. Arbitrary DELETE/DDL injection is outside this scope.
PostgreSQL constraints, triggers, and transaction semantics MUST remain active.

Injection MUST record the intended operation before execution and its committed
outcome afterward, including target and relevant before/after values. A failed
post-commit capture MUST invoke the incomplete-evidence policy, not claim that
the mutation was undone. Management connections MUST bypass response injection
and have only the access needed for their authorized operation.

The source schema/seed assets MUST never be modified. Disabling or consuming an
injection MUST NOT undo an already committed write. Fresh sessions MUST start
from sealed seeds; snapshot restore MUST recover the injected state present at
that checkpoint. Persistent writes MUST NOT be replayed as fresh application SQL
during restore, which could repeat triggers or other effects.

## Database-response injection

Response injection MUST use the existing `simulation` lifecycle and placement
machinery with an explicitly documented database-response surface. It MUST run
after backend execution and before the selected result reaches the application.
It MUST NOT rewrite submitted SQL, bypass authorization, change stored records,
or fabricate command/transaction success. Ordinary effects of the original SQL,
including writes with RETURNING, remain real and MUST be recorded independently.

Selectors MUST support service, a bounded query identity or query-text predicate,
typed bound-parameter predicates, and existing call ordinals. Placement MUST
select a result set, row (bounded ordinal or exact returned-value predicates),
and column, with a JSON pointer where applicable. Duplicate column names MUST
require an unambiguous ordinal. The contract MUST define matching/counting across
multiple results, prepared executions, portal and SQL-cursor fetches, and
concurrent connections.
It MUST NOT claim a stable row order when SQL does not provide one.

The first implementation MUST support text/varchar and JSON/JSONB payload fields
in text and binary result formats. It MUST preserve column metadata, SQL types,
NULL semantics, message lengths, and all nonselected values. It MUST NOT insert
arbitrary bytes into numeric or vector fields. pgvector and other nonselected
types MUST continue to pass through with their native encodings.

`once` and `standing` MUST follow existing response-injection consumption,
placement-failure, ordering, and disable semantics. Ordinals and consumption MUST
be concurrency-safe and included in snapshots. Observed-window attribution MUST
NOT introduce turn-scoped response selection or causal guarantees. Existing
scope limitations MUST remain explicit.

Both modes MUST feed the existing delivery-confirmation machinery. Persisting a
payload or rewriting a response is not confirmed model delivery. Confirmation
MUST require observing the injected content in the final outbound model request
under existing rules. If application filtering, caching, validation, or
summarization prevents confirmation, the result MUST remain unconfirmed rather
than inferred from a successful database read. Native application behavior,
including digest checks that reject an altered document, MUST NOT be disabled.

## Committed-state oracles

Sealed database oracles MUST independently inspect actual committed state through
a private connection that bypasses response injection. They MUST support bounded
checks of record existence/absence and column/aggregate values sufficient to
verify outcomes such as a committed refund and its amount. Application roles
MUST NOT access oracle definitions, verdicts, or oracle credentials.

Oracle reads MUST use a consistent database view and bounded execution. Any
configured SQL MUST be sealed/reviewer-authored, read-only, and constrained by a
restricted role and time/result limits; adaptive callers MUST NOT supply SQL.
The implementation MUST capture query identity, observation boundary, and actual
result in integrity-covered evidence for retained/offline evaluation. Historical
export MUST NOT reconnect to a live database to invent missing evidence.

Oracles MUST distinguish harness-authored state injection from application
changes. An injected refund record alone MUST NOT be reported as an agent-created
refund. Likewise, returned rows or successful COMMIT alone MUST NOT establish a
specific business condition without the independent state check. Existing
cumulative/latched outcomes and causal-attribution limits MUST remain intact.
Evaluation failure or incomplete capture MUST be unavailable/error, not false.

## Coordinated snapshots and restore

Snapshots MUST include database contents/schema, pgvector data/indexes, required
roles/grants, injection definitions/progress, oracle state, and provenance using
the existing checkpoint/blob integrity and usage accounting. Engine/extension
compatibility MUST be recorded and verified before restoring. Private runtime
credentials MUST remain outside public snapshot/export views.

The coordinator MUST first block new application invocations and allow configured
application quiescence/state-save hooks to finish their database calls. It MUST
then prevent new database work and drain in-flight work to a bounded, consistent
boundary before capture. It MUST preserve the existing lock order and avoid
holding a service gate needed by the application's save hook.

Snapshots MUST NOT capture active transactions as if committed or silently commit
or discard application work. An unresolved transaction or failed quiescence MUST
reject the snapshot with an actionable error. Idle pooled connections MUST NOT
by themselves prevent a snapshot.

The implementation MUST use a PostgreSQL-supported consistent backup method, not
copy a live data directory as ordinary mutable files. It SHOULD choose the
simplest method compatible with coordinated quiescence and existing budgets.
All declared databases and application-state/filesystem captures MUST share the
same coordinated boundary. Backup bytes MUST count against existing snapshot
limits, and checkpoint readiness MUST wait for all required storage to succeed.

Restore MUST validate all artifacts before source closure, reconstruct a fresh
isolated backend, restore roles/data and injection state, and establish proxy
readiness before application initialization/state restoration. It MUST NOT
restore live database sockets, prepared statements, or open transactions.
Applications MUST reconnect through their ordinary initialization/pool lifecycle.
Earlier intact checkpoints MUST remain usable according to existing lifecycle
rules; incomplete restore MUST never be reported ready.

## Verification and acceptance

Focused tests MUST cover validation/sealing, unsafe assets, invalid grants and
selectors, resource limits, package identity, startup/stop failures, and isolation
between sessions. Tests MUST also cover:

- Verified TLS and hostname rejection, invalid credentials, declared auth modes,
  and attempts by the actual application identity to bypass the proxy, read
  database files, obtain management credentials, or access external resources.
- Real Psycopg and Node `pg` clients with connection pools, simultaneous
  independent transactions, rollback/savepoints, lock waits, cancellation,
  disconnect recovery, prepared statements, binary/text parameters/results,
  multiple results, portals, and pipeline synchronization where supported.
- CRUD, joins, unions, grouping, sorting/limits, constraints, catalogs, pgvector
  registration and similarity search, and explicit unsupported-flow errors.
- Persistent updates and allowed record creation, disallowed creation, duplicate
  keys, missing targets, type/constraint failures, lock races, and unchanged seeds.
- Response rewrites of text and JSON/JSONB in both formats, duplicate column
  names, unmatched queries, reused prepared statements, NULLs, multiple rows,
  placement failures, concurrent once consumption, and unchanged database state.
- Distinct original/rewritten evidence, late/outside/overlapping observed turns,
  multi-turn transactions, committed versus rolled-back outcomes, independent
  oracle reads, and unconfirmed versus confirmed model delivery.
- Faults before execution, after execution/commit, during result capture/rewrite,
  event append/flush, and snapshot storage. Verify blocked subsequent work,
  truthful uncertainty, partial exports, no automatic replay, and cleanup.
- Snapshot/restore with an idle pool, application save hooks that query the
  database, unresolved transactions, persistent injections, consumed response
  injections, pgvector state, oracle history, corrupted backups, and budget
  exhaustion. Verify restored state rather than merely backup-file existence.

The implementation MUST demonstrate the complete workflow in Docker using a
release-built CLI, a packaged wrapper, and real database drivers. Existing
qualification tools and application examples SHOULD be extended. The maintained
example MUST require no live external provider; use a deterministic model fixture.
It MUST:

1. Build from a sealed Blueprint and start the packaged database without host
   PostgreSQL, runtime downloads, or target source changes for interception.
2. Exercise pooled SQL and pgvector retrieval with TLS verification enabled.
   The Blueprint MUST seed related populated tables and document rows containing
   known vectors. Before any injection, verify seeded values, a join across the
   related tables, and expected nearest-neighbor results for a fixed query vector.
   Verify that a second fresh session starts with the original seeded records
   after the first session has changed them.
3. Arm a response payload, retrieve it through the ordinary driver, confirm its
   presence in an outbound model request, and independently prove the stored
   source record is unchanged. A nonmatching subsequent read MUST be benign.
4. Arm a persistent update and an explicitly permitted record creation, confirm
   committed state and later retrieval, and prove source seed assets are intact.
5. Execute one committed and one rolled-back business operation and verify that
   oracles and exported evidence distinguish them from harness-authored changes.
6. Snapshot, make further changes and consume response injections, restore, and
   verify database/application state and injection progress at the checkpoint.
7. Exercise a controlled required-evidence failure and verify explicit failure,
   blocked new work, preserved actual effects, and inspectable partial evidence.
8. Clean up all created containers, processes, sockets, and temporary resources.

Qualification MUST additionally exercise the current customer-support agent's
schema checks, Psycopg pool, pgvector lookup, and refund transaction path using
its normal invocation adapter. The implementation MUST NOT weaken its checks to
make tests pass or introduce agent-authored evidence dependencies. Unavailable
agent dependencies MUST be reported as a validation gap.

Linux AMD64 and ARM64 packaging and execution MUST be checked in available CI;
macOS qualification MUST exercise the existing Linux-companion wrapper build
path. The report MUST identify exact tested artifacts, engine/extension versions,
drivers, and architectures, and disclose unavailable platforms. Mocked tests
alone MUST NOT establish end-to-end database support.

## Documentation and handoff

The implementation MUST update Blueprint, installation/wrapper-building,
injection/evaluation, evidence, snapshot/restore, and relevant API/capability
documentation. A database integration guide and runnable examples MUST show
schema/seeds, connection credentials, verified TLS, both injection modes,
committed-state oracles, and snapshot/restore using normal commands.

Documentation MUST distinguish supported SQL execution from supported response
mutation types, and dynamic served content from persistent data. It MUST explain
pooling, transaction/cancellation limits, resource failures, authentication,
delivery confirmation, and independent oracle evidence. Public docs MUST describe
implemented behavior rather than implementation status or speculative features.
The handoff MUST report actual qualification results and remaining gaps.
