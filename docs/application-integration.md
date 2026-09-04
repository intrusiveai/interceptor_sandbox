# Application integration

Interceptor needs a small amount of information about an application before it
can run it safely. The Environment Blueprint supplies that information: how to
start the application, how a tester invokes it, which operations may be called,
where it stores data, and how it reaches the model and simulated services.

## Application image and wrapper image

You build the normal application image first. You do not need to add `sandboxd`
or Interceptor-specific listeners to that Dockerfile. After the Environment is
sealed, `interceptor app build` creates a second image—the wrapper image—by:

1. using the application image as its starting point;
2. adding the Linux `interceptor` executable; and
3. when local TLS is enabled, adding the generated public sandbox CA and
   applying the selected trust mode; and
4. making Interceptor the wrapper image's entrypoint.

When a session starts, that executable runs in `sandboxd` supervisor mode.
`sandboxd` starts the model proxy, simulated services, and attacker sinks. It
then prepares the selected application interface—either starting a non-root
application process or a non-root command helper—and opens the private bridge.
The host-side `interceptor run` process creates the local URLs printed for the
tester and owns the real model-provider connection.

The original image's `ENTRYPOINT`, `CMD`, and `USER` are not used to start the
application in a sandbox session. The Blueprint's `application.launch.command`
replaces the original startup command, and application code runs under
Interceptor's non-root application identity. For HTTP, stdio, REPL, and
external interfaces it is the long-running command; for `command` it is the
base argv used for each operation; for `code` it names the Python or Node
executable. Declare it explicitly, preferably using an absolute executable
path, and ensure the image's files are readable and executable by that identity.

Similarly, the application receives the environment variables declared in
`application.launch.environment`, declared static synthetic credentials,
identity-delivery variables, and Interceptor's virtual-time value. Environment
variables declared only with `ENV` in the original Dockerfile are not
automatically passed to the child application. Do not rely on the original
image entrypoint to assemble arguments or environment variables at startup.

See [How the application becomes a sandbox
image](architecture.md#how-the-application-becomes-a-sandbox-image) for the full
build and runtime sequence.

## Required application contract

The application must:

- be available as a container image locked to an exact digest (a digital
  fingerprint) ending in `@sha256:<64-lowercase-hex>`;
- start from a fixed command and working directory;
- use one declared application interface and expose every callable operation in
  `application.interface.operations`;
- direct all supported model calls to the local `MODEL_BASE_URL` supplied in
  the Blueprint;
- use one of the native model APIs supported by the selected host adapter; and
- avoid dependencies on undeclared external network services.

The container cannot use the general network. Any non-model HTTP dependency
must therefore be represented by a simulated service in the Blueprint. This is
what keeps a test from accidentally reaching a real production service.

An agent that normally uses a remote MCP server can instead point its MCP
client at a declared `mcp_http` service. Interceptor implements a deterministic,
non-streaming tool subset and records MCP tool activity. See [Simulated MCP
services](mcp-services.md) for the supported protocol and tool behaviors.

Set positive `startup_timeout_ms` and `turn_timeout_ms`. If `health_path` is
present for an HTTP application, the supervisor waits for a successful response
(status 200–299) before reporting the session as running. REPLs may instead
declare a startup prompt, and external/code adapters explicitly report ready.

The interface can match the application you already have. HTTP services keep
their native routes; a one-shot CLI can receive arguments and stdin; a
long-running stdin/stdout program or REPL can use delimiters or prompts; and a
Python or Node module can be imported and called directly. A small external
adapter protocol covers unusual invocation requirements. See [Application
interfaces](application-interfaces.md) for the configuration and tradeoffs.

## Environment variables and credentials

Application environment values are written directly in the Blueprint; they are
not copied from matching variables on the host. Expressions such as `${TOKEN}`
are rejected. `MODEL_BASE_URL` must point to a local address. Other names ending
in `_BASE_URL` may point only to a local address or a hostname belonging to a
simulated or attacker service declared in the Blueprint.

When `network.tls` is enabled, simulated HTTPS and MCP-over-HTTPS origins use a
private, build-generated CA. `auto` supplies common runtime trust variables;
`bundle` exposes `INTERCEPTOR_CA_BUNDLE` and the fixed
`/opt/interceptor/tls/ca.pem` path; and `application-managed` requires a
Blueprint probe command for custom stores such as a JVM truststore or pinned
runtime. Only the public CA enters the wrapper. The server keypair stays in
protected host data and is mounted for the supervisor at runtime.

Credential-looking variables—names containing `TOKEN`, `SECRET`, `PASSWORD`,
or ending in `_KEY`—must be declared in `credentials` with the same value and a
synthetic service or `model-proxy` in `accepted_by`. Use only synthetic secrets
here.

Real model-provider authentication is different. In production, the host
controller resolves a stable credential ID from AWS Secrets Manager, Azure Key
Vault, Google Cloud Secret Manager, or HashiCorp Vault, or uses the provider's
workload identity. A named host environment variable remains available only
for local development. Bedrock uses the native AWS SDK credential chain.
Interceptor never injects these credentials into the application container.

Interceptor does not require an application to convert its model protocol. It
supports native OpenAI Chat Completions and Responses, Azure OpenAI, Anthropic
Messages, Gemini Developer API and Vertex Gemini `generateContent`, Amazon
Bedrock Converse, and the two OpenAI-compatible LiteLLM routes. The selected
host adapter must match the application API. Cross-protocol translation is not
part of the MVP.

SDKs often insist on an API key or AWS signature before sending even to a local
endpoint. Give the application a declared synthetic value, never a real
credential. For Bedrock, point the SDK at the loopback proxy with
`AWS_ENDPOINT_URL_BEDROCK_RUNTIME` (or its explicit endpoint option) and provide
synthetic AWS access/secret values accepted by `model-proxy`. Interceptor
ignores the local signature, while the host AWS SDK signs the real call with its
own credential chain. See [Model providers](model-providers.md) for examples and
the supported native subsets.

An application that normally retrieves a workload identity can instead use an
`identity.delivery`. Interceptor can supply ordinary OAuth client settings, a
rotating projected JWT file, or the AWS SDK's standard
`AWS_CONTAINER_CREDENTIALS_FULL_URI` endpoint. Protected simulated HTTP and MCP
services validate the resulting short-lived synthetic token and the current
Blueprint grant.

For delegated access, an HTTP test request can carry
`X-Interceptor-Caller: <declared-user>`. Interceptor removes that harness-only
value and passes a signed user bearer token to the application. The agent can
exchange it with its own client identity for a downstream token that preserves
both the user and acting agent. Non-HTTP interfaces can request a caller token
from the control API or CLI and place it in the application's declared input
format. See [Synthetic identity and delegated access](identity.md).

## Snapshot integration

Only enable `snapshot_capable` if the application can restart correctly using:

1. its original image and read-only starting files;
2. the folders declared as writable application state (`mutable_roots`); and
3. state managed by Interceptor, such as simulated service records, active
   injections, identity grants, delegated consent, and issuer failure state.

All information that must survive a restart needs to be written under
`mutable_roots`. Information held only in memory, `/tmp`, an undeclared folder,
an open connection, or an external service is not included in a snapshot.

Before taking a snapshot, Interceptor asks the application to finish or save any
work that is still in progress. This is called **quiescence**. Declare one way
for Interceptor to request that safe stopping point:

- `http`: an application endpoint saves pending work and returns a successful
  HTTP status;
- `signal`: the application saves pending work after `USR1`, `USR2`, `HUP`, or
  `TERM` and signals readiness with a file or a successful URL response;
- `command`: a configured command saves pending work; or
- `none`: appropriate only when the application has no buffered persistent
  state and the declared filesystem can be scanned safely.

After the application reports that it is ready, Interceptor checks the saved
folders twice to make sure nothing is still changing. It then pauses the
application very briefly while it records the final changes. The quiescence
action should be safe to run more than once: if a snapshot cannot be completed,
the current session continues and the operator may try again later.

See [Snapshots and restore](snapshots-and-restore.md) for the complete model.

## Wrapper image workflow

After sealing the Blueprint:

```sh
interceptor app build --environment ./environment --tag my-sandbox:local
interceptor app verify --environment ./environment
```

The build records which exact application image and Sealed Environment were
used. If either one changes, verification detects the mismatch and requires a
new seal and build. This prevents a test from quietly running different code or
environment data than the reviewer approved.

The wrapper image is the image used for sessions. You do not run it manually;
`interceptor run` locates it through the recorded application lock and starts
its `sandboxd` mode with the required mounts, limits, and bridge configuration.

For a locally tagged source image, get the ID with:

```sh
docker image inspect --format '{{.Id}}' my-agent:local
```

Use the exact returned image fingerprint in `application.image`, for example
`my-agent:local@sha256:...`. The builder verifies that the local tag matches.

## Smoke-test checklist

Before relying on an environment:

1. Start the session and verify its configured readiness check.
2. Call every declared application operation through the application URL.
3. Confirm a model call succeeds through the proxy using an allowed model.
4. Exercise each synthetic service route and resolve every reported coverage gap.
5. Verify synthetic credentials work only with their declared service.
6. Trigger each oracle (a deterministic success/failure check) with a controlled
   positive case.
7. If snapshots are enabled, mutate both service and file state, snapshot,
   restore, and verify the application sees the restored values.
8. Confirm unsnapshotted in-memory state is not required after restart.
