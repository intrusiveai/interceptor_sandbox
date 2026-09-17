# Getting started

## Prerequisites

- Go 1.26 or newer
- a running Docker daemon; Docker Desktop is supported on macOS
- an application image built for the Docker host architecture
- host credentials for a supported model provider, or the included mock
  provider for a local smoke test

Build and test the project from the repository root:

```sh
make build
make test
make vet
```

The executable is `bin/interceptor`. Add it to `PATH` or use that path in the
examples below.

## Run the reference environment

The reference environment is a small working example containing an agent,
simulated support-ticket service, synthetic private data, an attacker capture
endpoint, and snapshot-capable application state. Running it once is the easiest
way to see how the pieces fit together before authoring a new Blueprint.

Build the reference application and get its exact local image ID:

```sh
docker build -t interceptor-reference-agent:local examples/reference-agent
docker image inspect --format '{{.Id}}' interceptor-reference-agent:local
```

The printed `sha256:...` value is the image's digital fingerprint. Open
`examples/reference-environment/environment.yaml` and find `application.image`
(currently line 13):

```yaml
image: interceptor-reference-agent:local@sha256:0000000000000000000000000000000000000000000000000000000000000000
```

Delete the placeholder value after `@` and paste the complete value printed by
`docker image inspect` in its place.

Next, update the review metadata near the top of the same file (currently lines
8–9):

```yaml
reviewed_by: your-name-or-handle
reviewed_at: 2026-08-25T18:30:00Z
```

Replace `your-name-or-handle` with the name or username of the person who
reviewed the Blueprint and its assets. Set `reviewed_at` to
the UTC date and time when that review was completed, using RFC 3339 format. The
timestamp above is only an example. These fields provide a local audit record;
they are not checked against an account service or another external value.

Then validate, seal, build, and verify:

```sh
interceptor environment validate examples/reference-environment
interceptor environment seal examples/reference-environment
interceptor app build \
  --environment examples/reference-environment \
  --tag interceptor-reference-sandbox:local
interceptor app verify --environment examples/reference-environment
```

The first `docker build` created the ordinary application image. The
`interceptor app build` command creates a second **wrapper image** from it. The
wrapper keeps the application and its dependencies, adds the Interceptor
executable, generates the reference environment's private TLS identity, embeds
only its public CA, and configures that executable to start `sandboxd` when a
session runs. The protected server keypair remains under Interceptor's host
data root. `sandboxd` starts the in-container DNS resolver, HTTP/HTTPS front
doors, model proxy, and simulated services,
prepares the Blueprint's selected application interface, waits for its declared
readiness condition, and then opens the private application and control bridge.
You do not need to add
`sandboxd` to the reference application's Dockerfile or run the wrapper image
yourself.

For a fuller explanation, see [How the application becomes a sandbox
image](architecture.md#how-the-application-becomes-a-sandbox-image).

In a separate terminal, start the included deterministic model provider:

```sh
go run ./examples/mock-provider
```

Start a session. The mock accepts any non-empty host credential, but the CLI
still requires one to exercise the real credential boundary:

```sh
export OPENAI_API_KEY='local-mock-key'
interceptor run \
  --environment examples/reference-environment \
  --model-provider openai-chat \
  --provider-base-url http://127.0.0.1:19090/v1 \
  --allow-model reference-model
```

`run` prints the native session ID and `http://127.0.0.1:8080`. Keep it running;
it owns the target and provider bridge. No separate gateway process is needed.

## Exercise the local integration

In another terminal, use the qualification client against this fresh reference
environment:

```sh
go run ./examples/local-api-smoke --output /tmp/local-api-evidence
```

The client attaches a test campaign, registers an artifact and attempts, invokes
the application, creates/restores a checkpoint, verifies fresh session identity
and stale-request rejection, then stops and exports both sessions. Add
`--data-root <absolute-test-data-root>` to verify application state rollback too.
The API remains available after target stop. Ctrl+C ends the host service.

To write another client, follow [Local Operator integration](local-api.md) and
the [native v1alpha2 wire format](cross-vm-gateway.md). No integration credential
is required. Native model-provider credentials stay on the host.

Administrator inspection and retained evidence remain available:

```sh
interceptor list
interceptor status <session-id> --json
interceptor snapshot list <session-id>
interceptor logs verify <session-id>
interceptor logs export <session-id> --output ./evidence.tar
```

## Create a Blueprint from scratch

```sh
interceptor environment init ./my-environment
```

This creates `my-environment/environment.yaml` and refuses to overwrite an
existing file. Edit the placeholder image, application interface and
operations, persistence declaration, and review metadata; add any referenced
assets below the same directory. The generated file uses HTTP, but an existing
CLI, REPL, Python module, or Node module does not need to be converted into an
HTTP server. Choose the matching configuration from [Application
interfaces](application-interfaces.md). Continue with `environment validate`,
`environment seal`, and `app build`.

See [Application integration](application-integration.md) before onboarding a
real target and [Environment Blueprint reference](environment-blueprint.md) for
the complete schema. [Model providers](model-providers.md) explains how to keep
the application's native Chat Completions, Responses, or Converse API while
routing it through the matching host connector.
