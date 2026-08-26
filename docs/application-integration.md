# Application integration

Interceptor needs a small amount of information about an application before it
can run it safely. The Environment Blueprint supplies that information: how to
start the application, where it listens, which routes may be called, where it
stores data, and how it reaches the model and simulated services.

## Application image and wrapper image

You build the normal application image first. You do not need to add `sandboxd`
or Interceptor-specific listeners to that Dockerfile. After the Environment is
sealed, `interceptor app build` creates a second image—the wrapper image—by:

1. using the application image as its starting point;
2. adding the Linux `interceptor` executable; and
3. making Interceptor the wrapper image's entrypoint.

When a session starts, that executable runs in `sandboxd` supervisor mode.
`sandboxd` starts the model proxy, simulated services, attacker sinks, and
private bridge. It then starts the application executable as a non-root child
process. The host-side `interceptor run` process creates the local URLs printed
for the tester and owns the real model-provider connection.

The original image's `ENTRYPOINT`, `CMD`, and `USER` are not used to start the
application in a sandbox session. The Blueprint's `application.launch.command`
replaces the original startup command, and the application runs under
Interceptor's non-root application identity. Declare the complete executable
and arguments, preferably using an absolute executable path, and ensure the
image's files are readable and executable by that identity.

Similarly, the application receives the environment variables declared in
`application.launch.environment` plus declared synthetic credentials and
Interceptor's virtual-time value. Environment variables declared only with
`ENV` in the original Dockerfile are not automatically passed to the child
application. Do not rely on the original image entrypoint to assemble arguments
or environment variables at startup.

See [How the application becomes a sandbox
image](architecture.md#how-the-application-becomes-a-sandbox-image) for the full
build and runtime sequence.

## Required application contract

The application must:

- be available as a container image locked to an exact digest (a digital
  fingerprint) ending in `@sha256:<64-lowercase-hex>`;
- start from a fixed command and working directory;
- listen on a local HTTP address inside the container;
- expose every externally callable method/path in `served_routes`;
- direct all supported model calls to the local `MODEL_BASE_URL` supplied in
  the Blueprint;
- use the OpenAI-compatible chat-completions API for model calls; and
- avoid dependencies on undeclared external network services.

The container cannot use the general network. Any non-model HTTP dependency
must therefore be represented by a simulated service in the Blueprint. This is
what keeps a test from accidentally reaching a real production service.

Set positive `startup_timeout_ms` and `turn_timeout_ms`. If `health_path` is
present, the supervisor waits for a successful HTTP response (status 200–299)
before reporting the session as running.

## Environment variables and credentials

Application environment values are written directly in the Blueprint; they are
not copied from matching variables on the host. Expressions such as `${TOKEN}`
are rejected. `MODEL_BASE_URL` must point to a local address. Other names ending
in `_BASE_URL` may point only to a local address or a hostname belonging to a
simulated or attacker service declared in the Blueprint.

Credential-looking variables—names containing `TOKEN`, `SECRET`, `PASSWORD`,
or ending in `_KEY`—must be declared in `credentials` with the same value and a
synthetic service in `accepted_by`. Use only synthetic secrets here.

The real model provider API key is different. The CLI reads it from a host
environment variable selected by `--model-api-key-env` and never injects it into
the application container.

## Snapshot integration

Only enable `snapshot_capable` if the application can restart correctly using:

1. its original image and read-only starting files;
2. the folders declared as writable application state (`mutable_roots`); and
3. state managed by Interceptor, such as simulated service records and active
   injections.

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

1. Start the session and verify its health check.
2. Call every declared application route through the application URL.
3. Confirm a model call succeeds through the proxy using an allowed model.
4. Exercise each synthetic service route and resolve every reported coverage gap.
5. Verify synthetic credentials work only with their declared service.
6. Trigger each oracle (a deterministic success/failure check) with a controlled
   positive case.
7. If snapshots are enabled, mutate both service and file state, snapshot,
   restore, and verify the application sees the restored values.
8. Confirm unsnapshotted in-memory state is not required after restart.
