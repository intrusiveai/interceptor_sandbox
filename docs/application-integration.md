# Application integration

Interceptor does not discover an application's behavior dynamically. Onboarding
is an explicit contract between the application image and its Environment
Blueprint.

## Required application contract

The application must:

- be available as an immutable OCI image reference ending in
  `@sha256:<64-lowercase-hex>`;
- start from a fixed argv and absolute working directory;
- listen on an HTTP loopback URL inside the container;
- expose every externally callable method/path in `served_routes`;
- direct all supported model calls to the loopback `MODEL_BASE_URL` supplied in
  the Blueprint;
- use OpenAI-compatible chat completions for the MVP model seam; and
- avoid dependencies on undeclared external network services.

The container has no general network. Any non-model HTTP dependency must be
represented as a declared synthetic service and referenced by a declared
`*_BASE_URL` hostname, or use loopback.

Set positive `startup_timeout_ms` and `turn_timeout_ms`. If `health_path` is
present, the supervisor waits for a 2xx response before publishing the session
as running.

## Environment variables and credentials

Application environment values are literal Blueprint content. Host interpolation
such as `${TOKEN}` is rejected. `MODEL_BASE_URL` must be an HTTP loopback URL.
Other names ending in `_BASE_URL` may point only to loopback or a hostname
declared by a synthetic or attacker service.

Credential-looking variables—names containing `TOKEN`, `SECRET`, `PASSWORD`,
or ending in `_KEY`—must be declared in `credentials` with the same value and a
synthetic service in `accepted_by`. Use only synthetic secrets here.

The real model provider API key is different. The CLI reads it from a host
environment variable selected by `--model-api-key-env` and never injects it into
the application container.

## Snapshot integration

Only enable `snapshot_capable` if the application can restart correctly from:

1. the immutable image and read-only environment seeds;
2. every declared mutable root; and
3. Interceptor-managed service, injection, budget, oracle, and clock state.

All durable application writes must stay under `mutable_roots`. State held only
in memory, `/tmp`, an undeclared directory, an open socket, or an external
service is not captured.

Declare one quiescence strategy:

- `http`: an application endpoint flushes pending work and returns 2xx;
- `signal`: the application flushes after `USR1`, `USR2`, `HUP`, or `TERM` and
  signals readiness with a file or a 2xx URL;
- `command`: an argv executed through the restricted application helper performs
  the flush; or
- `none`: appropriate only when the application has no buffered persistent
  state and the declared filesystem can be scanned safely.

After quiescence, Interceptor also requires two matching filesystem scans and
briefly pauses the application for the final delta. A quiesce hook should be
idempotent because a failed snapshot leaves the current session running.

See [Snapshots and restore](snapshots-and-restore.md) for the complete model.

## Wrapper image workflow

After sealing the Blueprint:

```sh
interceptor app build --environment ./environment --tag my-sandbox:local
interceptor app verify --environment ./environment
```

The build records a separate application lock below Interceptor's data root. It
binds the wrapper image and digest to the Sealed Environment and source
application digest. Changing the Blueprint or source image requires resealing
and rebuilding.

For a locally tagged source image, get the ID with:

```sh
docker image inspect --format '{{.Id}}' my-agent:local
```

Use the exact returned digest in `application.image`, for example
`my-agent:local@sha256:...`. The builder verifies that the local tag matches.

## Smoke-test checklist

Before relying on an environment:

1. Start the session and verify its health check.
2. Call every declared application route through the application URL.
3. Confirm a model call succeeds through the proxy using an allowed model.
4. Exercise each synthetic service route and resolve every reported coverage gap.
5. Verify synthetic credentials work only with their declared service.
6. Trigger each oracle with a controlled positive case.
7. If snapshots are enabled, mutate both service and file state, snapshot,
   restore, and verify the application sees the restored values.
8. Confirm unsnapshotted in-memory state is not required after restart.

