# Spool file management and evidence purge

The normative rules are [spool management](../specs/mvp.md#432-host-spool-file-management)
and [purge](../specs/mvp.md#87-administrative-evidence-purge).

## Spool size and shutdown

`interceptor run --spool-max-bytes <bytes>` overrides
`INTERCEPTOR_SPOOL_MAX_BYTES`; the default is 536870912 bytes (512 MiB). Values
must be positive integers. Persist the effective limit in `session.json` and keep
it through managed restores. This host setting does not modify the sealed Blueprint.

The host checks the combined HTTP/model spool size every second, counting all file
lengths, including temporary, pending-deletion and unexpected nested files. It does
not follow links. Entries disappearing during cleanup are harmless; other scan errors
or a scan exceeding five seconds close admission and terminate through Docker.
Exceeding the size limit does the same with reason `SPOOL_SIZE_LIMIT`, retaining
uncertain outcomes and the integrated API's terminal closure. The check is periodic,
so size can overshoot before shutdown. The native Unix transport has no spool check.

Delete whole spool directories after confirmed container exit and stopped bridge
writers on normal stop, failure or partial startup. Preserve evidence. At startup,
reclaim abandoned spools only after proving their containers/writers inactive;
report unresolved cleanup instead of reusing their files. A target restore gets a
fresh empty spool and cleans up the source session's spool.

## Evidence groups

Each `sessions/<session-id>/` contains its own journals, logs, checkpoints, mutable
generations, artifacts, receipts, `blobs/` and managed `exports/`. A restore copies
the verified logical journal prefix, required checkpoint/provenance records and all
referenced blobs into the new session before readiness. Purging a source session
cannot break a retained replacement. Digest-based reuse stays within a session.
Keep the campaign ID in attached/restored session metadata for campaign selection.
Reusable images, sealed inputs, application locks and TLS material remain installation
resources. Completed session evidence stays until an administrator purges it.

## Commands

```sh
interceptor purge --session <session-id>
interceptor purge --campaign <campaign-id>
interceptor purge --all
```

Use exactly one selector. Campaign purge includes all its restored sessions; all
includes unassociated sessions too. `interceptor delete <session-id>` uses the same
semantics as session purge. These commands remove all managed evidence in the selected
groups and update durable and in-memory instance/session inventories, including
associated lifecycle records. The MVP holds a shared maintenance lock for the
lifetime of `interceptor run` and while evidence commands run. Exit the local API
service before purging; purge refuses when it cannot acquire exclusive access.
No container is stopped implicitly. Before any deletion, refuse the selection if it includes active or
unconfirmed containers, bridge writers or ongoing reads, reports, exports or restores.
Use each session's host-owned Docker binding to verify its original daemon and
full container ID; context/name changes do not change the target. An unavailable
or changed daemon and missing identity/exit proof preserve all selected evidence.
Successful purge also removes the selected host bindings and exit receipts.
See [Docker identity and recovery](docker-identity.md).
Serialize the check and deletion with those operations. Partial filesystem failures
are reported with nonzero exit status and may be retried.

Purged evidence no longer supports reports, feedback reads or checkpoint restore.
Previously exported administrator copies remain. The commands preserve configuration,
credentials, installed images, sealed environments and build/TLS material. Session
and campaign purge preserve the separate host credential-resolution audit; `--all`
also removes that audit after preflight confirms its writers are inactive.
Interceptor purge does not purge Operator's retained copies. Operator has its own `operatorctl purge --campaign <id>` and `--all` commands.
