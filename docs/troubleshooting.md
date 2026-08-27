# Troubleshooting

## Start with these checks

```sh
interceptor environment inspect <environment> --json
interceptor app verify --environment <environment> --json
interceptor list --json
interceptor status <session-id> --json
docker ps -a --filter name=interceptor-
```

For a failed startup, inspect the session's `termination_reason`, Docker logs,
and `logs/application.stderr.log` under `INTERCEPTOR_DATA_DIR` (or the platform
default data root).

## Blueprint does not validate

Validation is intentionally strict because an incomplete Blueprint could let a
test contact the wrong service, save incomplete state, or produce misleading
evidence. The error usually identifies the exact field; the list below explains
the most common underlying problems.

Common causes:

- the image has a changeable tag but is not locked to an exact fingerprint as
  `name@sha256:<digest>`;
- `reviewed_by` or `reviewed_at` is missing;
- `MODEL_BASE_URL` or `application.interface.http.listen` is not a local-only
  HTTP address;
- a `*_BASE_URL` hostname points somewhere other than a declared simulated
  service;
- a credential-looking variable lacks a matching synthetic credential entry;
- a writable seed is outside `mutable_roots`;
- mutable roots overlap or use a reserved system path;
- a referenced YAML file has an unknown field or a second document;
- a fixture rule is ambiguous at the same priority;
- a stateful action has the wrong placeholder, `key_field`, or `patch_fields`;
- a canary's declared file does not contain its value; or
- an asset path, symlink, hard link, or special file violates the sealed-directory
  boundary.

Use `environment validate --json` to inspect the asset list once validation
succeeds.

Blueprints from the earlier alpha schema used `application.io`, `adapter`,
`application_listen`, and `served_routes`. Those fields are intentionally not
accepted as aliases because sealing must have one unambiguous format. Move them
to `application.interface`, `kind`, `http.listen`, and named `operations` as
shown in [Application interfaces](application-interfaces.md), then seal and
rebuild the wrapper image.

## Wrapper image is missing or stale

`run` requires a matching application lock. After any Blueprint, asset, or
application-image change:

```sh
interceptor environment seal <environment>
interceptor app build --environment <environment> --tag <wrapper-tag>
interceptor app verify --environment <environment>
```

For a local source tag, ensure `application.image` uses the exact current value
from `docker image inspect --format '{{.Id}}' <tag>`.

## Docker is unavailable

Confirm Docker Desktop or the Docker daemon is running and that the current user
can execute `docker info`. Unit tests can run without Docker, but `app build`,
`app verify`, `run`, and `restore` need the daemon.

## Application readiness times out

Check that:

- `launch.command` and `workdir` exist in the application image;
- an HTTP application binds exactly to `application.interface.http.listen`, not
  `0.0.0.0` or a different port, and `health_path` returns 2xx without external
  dependencies;
- a stdio/REPL `ready_pattern` matches the text actually written by the
  application, including spaces at the end of a prompt;
- a code module can be imported from `workdir`, or an external adapter writes a
  valid `ready` frame to stdout;
- `startup_timeout_ms` is long enough and positive;
- the application user can read its image content and writable seed; and
- application logs do not show an undeclared network or file dependency.

## Application operation hangs or ends too early

For stdio and REPL applications, compare the captured application output with
the configured completion rule. Prefer a definite prompt or delimiter over
`idle`, which can mistake a natural pause for the end of a response. A PTY may
echo submitted input; enable `strip_echo` if that text should not be returned.

For commands, confirm the input is valid for the selected `stdin` mode, every
JSON Pointer used by `arguments.from` exists, and the actual exit status appears
in `success_exit_codes`. For external adapters, stdout must contain only one
JSON protocol frame per line; send diagnostics to stderr and correlate results
by invocation ID.

## Model calls fail

Verify:

- `turn_timeout_ms` is positive;
- the application calls `MODEL_BASE_URL`, not the provider directly;
- it uses a supported Chat Completions, Responses, or Converse path;
- the request's body `model` (OpenAI) or path `modelId` (Converse) exactly
  matches one repeated `--allow-model` value;
- the selected `--model-provider` is correct;
- for OpenAI or LiteLLM, `--provider-base-url` includes the intended base path
  but no credentials, query, or fragment;
- for OpenAI, `OPENAI_API_KEY` or the selected `--model-api-key-env` is
  non-empty; for an authenticated LiteLLM gateway, its selected key is
  non-empty;
- for Bedrock, an AWS region is available, the host credential chain is valid,
  and the identity has `bedrock:InvokeModel` permission; and
- the provider is reachable from the host. HTTP providers must not redirect to
  another origin.

Check that the application's API matches the selected adapter. A Responses call
cannot use `openai-chat`, and a Converse call cannot use `openai-responses`.
All current routes reject streaming. For Bedrock, also verify that the
application SDK points to the loopback endpoint and uses declared synthetic
credentials rather than trying to reach AWS directly. See [Model
providers](model-providers.md) for each adapter's supported subset.

For local diagnosis, use the included mock provider and `reference-model` to
separate application integration problems from upstream provider behavior.

## MCP tool calls fail

Verify that the MCP client uses the declared `http://...sandbox.local:<port>/mcp`
URL rather than the real remote endpoint. Then check that the service has
`kind: mcp_http`, its `endpoint` matches exactly, and the requested tool and
arguments have a matching definition.

HTTP `GET` streams, SSE, sessions, interactive OAuth login/PKCE, resources,
prompts, and server-initiated MCP messages are not supported in the MVP. A
service may require an already obtained Interceptor synthetic bearer token; if
so, verify its `authorization`, grant/delegation, audience, and scopes. An
unknown method, tool, or fixture input is reported as a coverage gap. See
[Simulated MCP services](mcp-services.md) for the exact supported subset.

## Synthetic identity fails

For autonomous access, confirm that the application received the declared
OAuth client and token-URL environment variables, requested the protected
service's exact audience and scopes, and has an active direct grant. For
delegated access, also confirm that:

- `identity.inbound.audience` names the application agent principal;
- the harness caller is a declared user owned by the same issuer;
- the agent exchanges the caller token while authenticating with its own client
  credentials; and
- an active delegation covers that user, agent, service, and requested scopes.

An intentionally injected inactive grant, revoked delegation, unavailable
issuer, rejected exchange, or expired token produces the same kind of failure
the application would receive from an identity system. Inspect evaluation
events for the synthetic identity decision; raw tokens and secrets are never
recorded.

## Unix bridge fails on macOS or Docker Desktop

This is expected. Unix sockets depend on an operating-system communication
endpoint that Docker Desktop's shared filesystem cannot reliably carry between
macOS and its Linux virtual machine. Remove `--bridge-transport unix` or set
`--bridge-transport spool`, which uses portable regular files.

Unix mode has no automatic fallback. On Linux, confirm the Docker daemon is
local, the session directory is a native shared filesystem, and the CLI and
container share the same kernel before selecting it.

## File spool request times out

Check whether the attached `run`/`restore` host process is still alive; it owns
the relays and provider connector. Then inspect session phase and container
state. A canceled caller may leave short-lived request files briefly, but
cancellation handling and cleanup should remove them without repeating an
action that changes state.

If the process or machine crashed, restart from a completed snapshot rather than
assuming a request that was in progress finished. Never manually move a request
file back into the pending directory; doing so could repeat an action that
already changed state.

## Synthetic service returns `501 coverage_gap`

The request did not match a sealed HTTP fixture, stateful/sink route, MCP
method, tool, or MCP fixture. This is a modeling gap, not a transient upstream
failure:

```sh
interceptor coverage <session-id>
interceptor logs tail <session-id> --channel evaluation
```

Add the missing deterministic fixture, route, or MCP tool behavior; review it;
then reseal, rebuild, and start a new session. Do not add general network as a
fallback.

## Snapshot returns `409 snapshot_failed`

Likely causes are:

- `snapshot_capable` is false or the snapshot limit is exhausted;
- the application did not confirm that it finished saving pending work;
- the command or signal readiness condition timed out;
- a background task kept changing a saved folder while Interceptor was checking
  it;
- changed content exceeded the configured size limit; or
- an unsupported file type appeared in a mutable root.

Fix the application's save/flush behavior rather than retrying indefinitely. A
failed request does not create a usable checkpoint. Check the current session
logs and try again only after the application can reach a known stable state.

## Restore refuses a checkpoint

Restore refuses to guess or ignore missing state. It stops when:

- the checkpoint belongs to another session;
- the Environment directory or wrapper lock changed;
- a saved history entry, checkpoint, file list, or saved file body is missing or
  modified;
- the checkpoint's Interceptor version or journal schema differs from the
  running binary; or
- rebuilt files do not match the fingerprint recorded in the snapshot.

Run `interceptor logs verify <source-session-id>`. Keep the original Sealed
Environment, application lock/image, source state history, checkpoint, and
referenced saved file content together for as long as restore is required.

## Evidence export is refused

Exports require a `stopped` or `error` session. Stop a live session first, then:

```sh
interceptor logs verify <session-id>
interceptor logs export <session-id> --output ./evidence.tar
```

If verification fails, preserve the data root for investigation. Do not edit the
line-oriented JSON history files or checkpoint JSON to force an export.
