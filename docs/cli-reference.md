# CLI reference

## Global behavior

```text
interceptor <command>
```

Commands print errors to stderr and return nonzero on failure. Commands with
`--json` produce machine-readable JSON where noted. There is no global config
file or global flag parser.

`INTERCEPTOR_DATA_DIR` overrides the platform user configuration directory used
for session data, application locks, and blobs. `INTERCEPTOR_PROVIDER_BASE_URL`
can supply the default provider URL for `run` and `restore`.

## Version and help

```sh
interceptor version
interceptor help
```

`--version` and `-version` are aliases for `version`; `-h` and `--help` are
aliases for `help`.

## Environment commands

```sh
interceptor environment init <directory>
```

Creates a starter `environment.yaml`. It creates the directory if needed and
refuses to overwrite an existing Blueprint.

```sh
interceptor environment validate <directory> [--json]
```

Strictly validates the Blueprint and assets without writing a lock. JSON output
contains `valid`, `name`, `version`, the asset manifest, and warnings.

```sh
interceptor environment seal <directory> [--json]
```

Validates and writes `environment.lock.json`. JSON output contains the lock and
warnings.

```sh
interceptor environment inspect <directory> [--json]
```

Verifies the directory against its existing lock. JSON output includes the lock,
parsed Blueprint, and warnings.

## Wrapper image commands

```sh
interceptor app build \
  --environment <directory> \
  --tag <wrapper-image> \
  [--json]
```

Builds an application-specific wrapper image and records its lock under the
data root. `--tag` is required.

```sh
interceptor app verify \
  --environment <directory> \
  [--image <expected-wrapper-image>] \
  [--json]
```

Verifies the Sealed Environment, application lock, wrapper image, and bound
digests. `--image` optionally asserts the expected wrapper image reference.

## Start a session

```sh
interceptor run \
  --environment <directory> \
  --provider-base-url <http-or-https-url> \
  --allow-model <model> \
  [--allow-model <model> ...] \
  [--model-api-key-env OPENAI_API_KEY] \
  [--app-port 0] \
  [--control-port 0] \
  [--bridge-transport spool|unix]
```

Required:

- `--environment` identifies a sealed, built environment.
- `--provider-base-url` fixes the provider origin and base path. It defaults
  from `INTERCEPTOR_PROVIDER_BASE_URL`.
- at least one `--allow-model` restricts request body model names.
- the host environment variable selected by `--model-api-key-env` must be
  non-empty. Its default is `OPENAI_API_KEY`.

Ports default to `0`, allowing the OS to choose unused loopback ports. The
bridge defaults to portable `spool`. Select `unix` only on a compatible native
Linux Docker host; see [Architecture](architecture.md#hostcontainer-transports).

The process stays attached because it owns the loopback relays and model
connector. Interrupting it requests a graceful stop.

## Inspect sessions

```sh
interceptor list [--json]
interceptor status <session-id> [--json]
```

`list` reads all session metadata below the configured data root. `status`
reads one persisted session record. These commands do not require the container
to be running.

Session phases are `creating`, `starting`, `running`, `quiescing`, `stopping`,
`stopped`, and `error`.

## Stop and delete

```sh
interceptor stop <session-id>
```

Requests an orderly supervisor stop, stops and removes the container, and marks
the session stopped. Repeating it for a stopped session is harmless.

```sh
interceptor delete <session-id>
```

Deletes a stopped or error session directory. Running sessions are refused.
Shared content-addressed blobs are retained, so deletion is not a secure-erasure
or garbage-collection command.

## Snapshots and restore

```sh
interceptor snapshot create <session-id> [--name <label>] [--json]
interceptor snapshot list <session-id> [--json]
interceptor snapshot inspect <session-id> <checkpoint-id>
```

Create asks the running supervisor for a quiescent logical checkpoint. Inspect
always prints checkpoint JSON.

```sh
interceptor restore <source-session-id> \
  --checkpoint <checkpoint-id> \
  --provider-base-url <url> \
  --allow-model <model> \
  [--allow-model <model> ...] \
  [--model-api-key-env OPENAI_API_KEY] \
  [--app-port 0] \
  [--control-port 0] \
  [--bridge-transport spool|unix] \
  [--stop-source]
```

Restore verifies and replays the checkpoint into a new attached session.
Provider and transport choices are supplied again because credentials are not
stored and a restored session may intentionally select either supported
transport. `--stop-source` stops the source only after the restored session is
healthy.

## Evaluation and coverage

```sh
interceptor evaluate <session-id> [--json]
interceptor coverage <session-id>
```

Both require a running session. `evaluate` prints the full JSON report. The
optional flag is accepted for consistency; output is JSON either way.
`coverage` prints only `coverage_gaps` and their count as JSON.

## Logs and evidence

```sh
interceptor logs tail <session-id> [--channel attack|control|evaluation]
```

Verifies the event chain and prints existing matching events as JSONL. This is
a current-file read, not a continuously following `tail -f`. The default
channel is `attack`.

```sh
interceptor logs verify <session-id>
```

Verifies event and journal hash chains, referenced blobs, checkpoints, session
identity, and Sealed Environment/application digests.

```sh
interceptor logs export <session-id> --output <tar-path>
```

Exports a verified, portable tar archive. The session must be `stopped` or
`error`. Existing destination-file replacement follows the host filesystem's
rename behavior, so choose the output path deliberately.

## Internal commands

`sandboxd` and `app-helper` are wrapper-image entry points used by Interceptor.
They are not user-facing administration commands and should not be invoked
directly.

