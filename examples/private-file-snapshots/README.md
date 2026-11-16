# Private-file snapshot qualification

This disposable command target creates nested 0700 directories and a 0600 file
under `/state/app`, alongside a supervisor-owned seed file. The existing native
API smoke client captures an empty baseline, creates and captures private state,
resets to the baseline, restores private state, reads and updates it through the
application, and captures/restores the update again. It checks exact modes,
logical ownership, and that bootstrap capabilities are absent during execution.
It also checks existence, content, digest and nested-absence oracle verdicts in
checkpoint state and final evidence before and after reset/restore.

With Docker running and `python:3.12-slim` available locally:

```sh
go build -o /tmp/interceptor-private-cli ./cmd/interceptor
go build -o /tmp/interceptor-private-client ./examples/local-api-smoke
python3 scripts/qualify_private_snapshots.py \
  --cli /tmp/interceptor-private-cli --client /tmp/interceptor-private-client \
  --output ../maintainers/interceptor_sandbox/private-snapshot-qualification
```

Run from the checkout for a development CLI. `--cli` can instead name an
extracted release's executable; keep its companion and metadata together.
Use `--transport unix` on a Linux host; macOS/Docker Desktop uses the default
spool transport. Both paths use a non-root supervisor and the separate application
UID. The script removes its test containers/images and purges its isolated
sessions and their mutable-root volumes on both platforms through normal product
purge, without repairing private-file permissions.
Evidence and result JSON remain in the requested output directory.

Run again with `--oracles-only` and a different output directory to verify private
file oracles and final evidence when the Blueprint disables snapshots.
