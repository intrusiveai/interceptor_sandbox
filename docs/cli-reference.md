# CLI reference

## Global behavior

```text
interceptor <command>
```

Commands print errors to stderr and return nonzero on failure. Commands with
`--json` produce machine-readable JSON where noted. There is no global config
file or global flag parser.

This page is primarily a lookup reference. For a guided first run, use
[Getting started](getting-started.md).

`INTERCEPTOR_DATA_DIR` overrides the platform user configuration directory used
for session groups and application locks. `INTERCEPTOR_PROVIDER_BASE_URL`
can supply the default provider URL for `run`; managed restore retains the original configuration.

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

Checks the Blueprint and all referenced files without changing them. JSON output
contains `valid`, `name`, `version`, a list of approved assets, and warnings.

```sh
interceptor environment seal <directory> [--json]
```

Validates and writes `environment.lock.json`. JSON output contains the lock and
warnings.

```sh
interceptor environment inspect <directory> [--json]
```

Checks that the directory still matches the version that was sealed. JSON
output includes the lock record, parsed Blueprint, and warnings.

## Wrapper image commands

```sh
interceptor app build \
  --environment <directory> \
  --tag <wrapper-image> \
  [--json]
```

Builds an application-specific wrapper image and records its lock under the
data root. `--tag` is required. If the sealed Blueprint enables local TLS, this
also generates the sandbox CA/server certificate, embeds only the public CA,
stores the server keypair under private host application data, and records all
material digests in the application lock.

```sh
interceptor app verify \
  --environment <directory> \
  [--image <expected-wrapper-image>] \
  [--json]
```

Checks that the Sealed Environment and wrapper image still match the exact files
and application image used during the build. `--image` can additionally require
a particular wrapper image name.

## Start a session

```sh
interceptor run \
  --environment <directory> \
  [--model-provider <adapter>] \
  --allow-model <model> \
  [--allow-model <model> ...] \
  [--provider-base-url <http-or-https-url>] \
  [--model-api-key-env <variable>] \
  [--model-credential-id <id> --credential-config <file>] \
  [--provider-auth none|api-key|workload-token|workload-identity] \
  [--provider-api-version <version>] \
  [--feedback-profile black-box|diagnostic|oracle-assisted] \
  [--production] \
  [--aws-region <region>] \
  [--aws-profile <profile>] \
  [--bridge-transport spool|unix] \
  [--spool-max-bytes <bytes>]
  [--evidence-max-bytes <bytes>]
```

`--spool-max-bytes` takes a positive integer, overriding
`INTERCEPTOR_SPOOL_MAX_BYTES`; default 536870912 bytes (512 MiB). The host checks
combined HTTP/model spool file lengths every second and terminates through Docker
on excess. Managed restore retains the effective value. See the [file-management rules](file-management.md).

Required:

- `--environment` identifies a sealed, built environment.
- at least one `--allow-model` restricts request body model names.

Provider options:

- `--model-provider` defaults to `openai-chat`. Supported choices also include
  `openai-responses`, `bedrock-converse`, `litellm`, `anthropic-messages`,
  `gemini-api`, `vertex-gemini`, and `azure-openai`.
- `--provider-base-url` fixes the HTTP provider or gateway origin and base
  path. It is required except for Bedrock and defaults from
  `INTERCEPTOR_PROVIDER_BASE_URL`.
- Model keys SHOULD use `--model-credential-id` with `--credential-config`.
  Explicit `--provider-auth api-key-env --model-api-key-env NAME` is supported
  in all modes; `--production` rejects only implicit environment lookup. Cloud
  identity selectors use the common authentication contract.
- LiteLLM supports explicit `--provider-auth none`; otherwise it sends no
  authorization header unless a host credential source is selected.
- Bedrock ignores API-key and base-URL options. `--aws-region` and
  `--aws-profile` optionally override values loaded by the AWS SDK's normal
  host configuration chain.

The application-facing route must match the provider choice: Chat Completions
for `openai-chat`, Responses for `openai-responses`, Converse for
`bedrock-converse`, and either supported OpenAI-compatible route for `litellm`.
See [Model providers](model-providers.md) for endpoint setup, authentication,
and current feature limits.

`--feedback-profile` chooses one profile declared by the sealed Environment and
cannot change during the session. The default is `black-box`.

The public API is fixed at `127.0.0.1:8080`; only one instance is supported.
The private bridge defaults to `spool`; select `unix` only on a supported native
Linux host. `run` stays attached after target stop for status/evidence. Ctrl+C
ends the service and any remaining target.

## Capabilities and integrated local API

```sh
interceptor capabilities --environment <directory>
```

Prints the deterministic manifest without starting a session. `interceptor run`
automatically serves the [local Operator API](local-api.md), including attach,
v1alpha2 experiments, snapshot restore, confirmed stop and evidence. There is no
separate `gateway` command, integration token or authentication configuration.

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

## Stop and purge

```sh
interceptor stop <session-id>
```

Stop the supervisor/container and host bridge writers, confirm exit and remove
transient spool directories. Retain journals and artifacts until explicit purge.
The command uses the original saved Docker socket and full container ID, verifies
the daemon/session identity, and reports unconfirmed cleanup if verification fails.
It does not use the CLI's current context to locate an existing session. See
[Docker identity and recovery](docker-identity.md).
An already stopped session still retries pending spool cleanup.

```sh
interceptor purge --session <session-id>
interceptor purge --campaign <campaign-id>
interceptor purge --all
interceptor delete <session-id>
```

Use exactly one purge selector. `delete` aliases session purge. Campaign purge
includes every associated restored session; all includes unassociated sessions.
Remove the selected groups' journals, logs, blobs, receipts, checkpoints, mutable
generations and managed exports. Preflight all selected groups and refuse active or
unconfirmed containers, bridge writers or ongoing read/report/export/restore work
before deleting any group. Preserve configuration, installation assets and explicit
external exports. `--all` also removes the host credential-resolution audit after
confirming its writers are inactive during preflight. Report IDs, deletion failures
and the loss of future reporting,
feedback and checkpoint restore. See [file management](file-management.md) for
complete rules.

## Snapshots and restore

```sh
interceptor snapshot create <session-id> [--name <label>] [--description <text>] [--campaign-id <id>] [--maximum-committed-bytes <bytes>] [--json]
interceptor snapshot list <session-id> [--json]
interceptor snapshot inspect <session-id> <checkpoint-id>
```

`--maximum-committed-bytes` optionally tightens the remaining native byte
allowance. It accepts nonnegative signed 64-bit integers; explicit zero rejects
creation as exhausted, while omission uses native limits. The allowance counts
all canonical file bytes in the candidate checkpoint, including reused content.

Create asks the application to reach a safe save point and records a logical
restore point. Inspect always prints the detailed checkpoint JSON.

Restore uses `POST /v1/lifecycle` on the running local service. Supply the bound
campaign, current session ID, stable operation ID, `operation: snapshot.restore`
and checkpoint ID. The source stops before the replacement starts. The response
returns a fresh native session and incremented run revision, with the original
provider/image/transport configuration. See [request examples](local-api.md).
The standalone `restore` command and `--stop-source` are no longer supported.
For a bound session, create snapshots through native `snapshot.create`; the
administrator inventory/inspection commands above remain available.
`--description` records the checkpoint purpose (valid UTF-8, at most 4,096 bytes).
`--campaign-id` optionally attributes an unassociated administrative snapshot;
it does not bypass campaign attachment. Bound snapshot creation derives campaign
ID from the native request envelope. `list` displays campaign and description;
`create --json`, `list --json` and `inspect` preserve both in checkpoint JSON.
Operator can list/inspect all retained campaign sessions through the local API.


## Synthetic caller identity

```sh
interceptor identity caller-token <session-id> \
  --principal <user-principal> \
  [--issuer <issuer-id>] \
  [--audience <audience>] \
  [--scope <scope> ...]
```

Requests a short-lived synthetic user token from a running session and prints
the JSON response. `--issuer` and `--audience` default from the Blueprint's
`identity.inbound` declaration. The principal must be a declared `user` owned
by that issuer. This command is useful when a non-HTTP application expects a
caller token inside its command or function input; HTTP applications can use
`X-Interceptor-Caller` instead.

The returned token is not valid outside the sandbox and is not saved in a
snapshot. See [Synthetic identity and delegated access](identity.md).

## Evaluation and coverage

```sh
interceptor evaluate <session-id> [--json]
interceptor coverage <session-id>
```

Both require a running session. `evaluate` prints the full JSON report. The
optional flag is accepted for consistency; output is JSON either way.
`coverage` prints only `coverage_gaps` and their count as JSON.

## Logs and evidence

Evidence export defaults to 4 GiB (4294967296 bytes) per session archive. The
`--evidence-max-bytes` flag on `run` and `logs export` overrides
`INTERCEPTOR_EVIDENCE_MAX_BYTES`. Values must be integer byte counts from 1 through
9007199254740991; zero is invalid. `run` fixes the API limit for that service instance
and advertises it in attach/status. CLI export selects its own limit, so a larger
one can recover retained evidence without restarting a campaign. The limit counts
the complete tar, including padding/trailer. Failure removes temporary output and
preserves the source evidence and any previous completed output.

```sh
interceptor logs tail <session-id> [--channel attack|control|evaluation]
```

Checks the event history for changes and prints existing matching events, one
JSON object per line. Despite the command name, this reads the current file once;
it does not continue waiting for new events. The default channel is `attack`.

```sh
interceptor logs verify <session-id>
```

Checks that the event history, state-change history, snapshots, and referenced
large files are complete and unchanged, and that they belong to the expected
session, application, and Sealed Environment.

```sh
interceptor logs export <session-id> --output <tar-path> [--evidence-max-bytes <bytes>]
```

Exports a verified, portable tar archive. The session must be `stopped` or
`error`. Existing destination-file replacement follows the host filesystem's
rename behavior, so choose the output path deliberately.

## Internal commands

`sandboxd` and `app-helper` are wrapper-image entry points used by Interceptor.
They are not user-facing administration commands and should not be invoked
directly.

## macOS host lifetime

`interceptor run` automatically manages `caffeinate -i -w <host-pid>` during active
execution, restore and cleanup. It releases the assertion while serving only retained
evidence. Screen lock is supported; actual sleep, logout or Docker interruption ends
execution. See [host lifetime and recovery](host-lifetime.md).

Host provider/store authentication, foreground execution, explicit environment keys
and GitHub OIDC MUST follow [the common auth contract](cloud-authentication.md).
Optional services run the same `run` command; see [host lifetime](host-lifetime.md).
