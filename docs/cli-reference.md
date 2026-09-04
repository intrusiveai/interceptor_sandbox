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
  [--app-port 0] \
  [--control-port 0] \
  [--bridge-transport spool|unix]
```

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
- Environment API keys are local-development compatibility. Production uses
  `--model-credential-id` with `--credential-config`, or a supported workload
  identity selected with `--provider-auth`. `--production` rejects environment
  credentials.
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

Ports default to `0`, allowing the OS to choose unused loopback ports. The
bridge defaults to portable `spool`. Select `unix` only on a compatible native
Linux Docker host; see [Architecture](architecture.md#hostcontainer-transports).

The process stays open while the session runs because it provides the local URLs
and the carefully limited connection to the model provider. Closing it requests
a graceful stop.

## Capabilities and Cross-VM gateway

```sh
interceptor capabilities --environment <directory>
```

Prints the deterministic CapabilityManifest without starting a session.

```sh
interceptor gateway <session-id> \
  --listen <host:port> \
  --auth-principal <id> \
  [--production --auth-token-credential-id <id> --credential-config <file> | \
   --auth-token-env <name>]
```

Serves typed Operator operations for one running session. In production this is
a private HTTP backend behind an enterprise-managed TLS load balancer. The
shared integration token comes from a supported host secret store and is used
only to issue, inspect, and revoke short-lived opaque execution leases. The
environment-variable source defaults to `INTERCEPTOR_GATEWAY_TOKEN`, is for
development only, and is rejected by `--production`. See [Cross-VM
gateway](cross-vm-gateway.md).

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

Deletes the local folder for a stopped or failed session. Running sessions are
refused. Shared large files are retained because another session may refer to
them, so this command neither securely erases all content nor reclaims all disk
space.

## Snapshots and restore

```sh
interceptor snapshot create <session-id> [--name <label>] [--json]
interceptor snapshot list <session-id> [--json]
interceptor snapshot inspect <session-id> <checkpoint-id>
```

Create asks the application to reach a safe save point and records a logical
restore point. Inspect always prints the detailed checkpoint JSON.

```sh
interceptor restore <source-session-id> \
  --checkpoint <checkpoint-id> \
  [--model-provider openai-chat|openai-responses|bedrock-converse|litellm|anthropic-messages|gemini-api|vertex-gemini|azure-openai] \
  --allow-model <model> \
  [--allow-model <model> ...] \
  [--provider-base-url <url>] \
  [--model-api-key-env <variable>] \
  [--model-credential-id <id> --credential-config <file>] \
  [--provider-auth none|api-key|workload-token|workload-identity] \
  [--provider-api-version <version>] \
  [--feedback-profile <profile>] \
  [--production] \
  [--aws-region <region>] \
  [--aws-profile <profile>] \
  [--app-port 0] \
  [--control-port 0] \
  [--bridge-transport spool|unix] \
  [--stop-source]
```

Restore checks the snapshot for missing or changed data, rebuilds its saved state
in a new session, and remains attached to that new session.
Provider, allowlist, credential-source, and transport choices are supplied
again because they are not stored as restorable credentials and a restored
session may intentionally select a different host provider or either supported
transport. `--stop-source` stops the source only after the restored session is
healthy.

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
interceptor logs export <session-id> --output <tar-path>
```

Exports a verified, portable tar archive. The session must be `stopped` or
`error`. Existing destination-file replacement follows the host filesystem's
rename behavior, so choose the output path deliberately.

## Internal commands

`sandboxd` and `app-helper` are wrapper-image entry points used by Interceptor.
They are not user-facing administration commands and should not be invoked
directly.
