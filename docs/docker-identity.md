# Docker identity and recovery

Interceptor resolves the administrator's Docker context or local `DOCKER_HOST`
once at run startup. It accepts local Unix sockets only, including Docker Desktop
and rootless Docker sockets. Later commands use the explicit saved endpoint and
ignore ambient Docker context/host/TLS overrides.

Each session has a host-owned `container-bindings/<session-id>.json` under
`INTERCEPTOR_DATA_DIR` (or the default data root). It records:

- Local Docker endpoint and daemon ID.
- Interceptor session ID.
- Full immutable Docker container ID.
- Whether creation was attempted.

This directory is never mounted into the container. `session.json.container_id`
also exposes the full ID for inspection, but cleanup trusts the host-owned record.
Interceptor creates the container with its session label, persists the full binding,
then starts it. It disables automatic restart and automatic removal.

## Stop, cleanup and purge

`interceptor stop <session-id>` uses the saved endpoint and ID, so renaming a
container or changing Docker contexts does not prevent termination. A replacement
container reusing the original name is untouched. The same binding rules apply
to quota/interruption termination, restore finalization, export preflight, abandoned
spool cleanup and purge. Container labels and daemon identity must match.

Before removing a stopped container, the host persists a `.json.exited` receipt
beside its binding. Retrying cleanup after removal requires this proof plus a
successful query of the original daemon. An absent container alone is insufficient.
A failed/unavailable daemon, identity mismatch or missing binding returns an error;
spools and evidence are retained until cleanup can be confirmed. Restore cannot
start a replacement after unconfirmed source cleanup.

If creation succeeded but the host lost the returned ID before persistence,
recovery can find exactly one container carrying that session label on the saved
daemon, verify it, and persist its full ID for cleanup. Recovery never starts it.
Zero or multiple matches remain unconfirmed. A launch recorded as never attempted
can be cleaned up without a container ID.

Successful session/campaign purge removes the associated bindings and exit
receipts as well as the evidence group. Normal shutdown retains them for recovery.

## Existing name-only sessions

Earlier builds did not persist enough information to identify the original daemon
and container reliably. The updated commands reject those sessions for destructive
cleanup rather than guessing from a name or the current Docker context. Finalize
and purge old sessions with the prior build on their original daemon before upgrading,
or manually reconcile their containers and retained files on that daemon. Missing
bindings are never automatically filled from the currently selected context.

See [validation](docker-identity-validation.md) for the exercised recovery cases.
