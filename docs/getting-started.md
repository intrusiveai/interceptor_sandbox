# Getting started

## Prerequisites

- Go 1.26 or newer
- a running Docker daemon; Docker Desktop is supported on macOS
- an application image built for the Docker host architecture
- credentials for an OpenAI-compatible model provider, or the included mock
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

The printed `sha256:...` value is the image's digital fingerprint. Copy it after
the `@` in
`examples/reference-environment/environment.yaml`. Also replace the example
`metadata.reviewed_by` value and confirm `reviewed_at` reflects the review.

Then validate, seal, build, and verify:

```sh
interceptor environment validate examples/reference-environment
interceptor environment seal examples/reference-environment
interceptor app build \
  --environment examples/reference-environment \
  --tag interceptor-reference-sandbox:local
interceptor app verify --environment examples/reference-environment
```

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
  --provider-base-url http://127.0.0.1:19090/v1 \
  --allow-model reference-model
```

`run` prints a session ID, application URL, and control URL and remains
attached. Keep it running. If a fixed local port is useful, add `--app-port`
and `--control-port`; otherwise the OS selects free loopback ports.

## Exercise the session

Use the printed application URL for the application's normal request shape:

```sh
curl -sS -X POST "$APPLICATION_URL/api/agent/invoke" \
  -H 'Content-Type: application/json' \
  --data '{"prompt":"List the open support tickets"}'
```

Or use the cooperative control URL to create a tracked turn. This wrapper gives
the request a turn ID and records a structured result. Because the wrapper can
carry any kind of request body, its `body` field uses base64 text (`e30=` is
the base64 form of `{}`):

```sh
curl -sS -X POST "$CONTROL_URL/v1/turns" \
  -H 'Content-Type: application/json' \
  --data '{
    "method":"POST",
    "path":"/api/agent/invoke",
    "headers":{"Content-Type":"application/json"},
    "body":"eyJwcm9tcHQiOiJMaXN0IHRoZSBvcGVuIHN1cHBvcnQgdGlja2V0cyJ9"
  }'
```

From another terminal, inspect and manage the session:

```sh
interceptor list
interceptor status <session-id> --json
interceptor evaluate <session-id>
interceptor coverage <session-id>
interceptor snapshot create <session-id> --name after-smoke-test
interceptor snapshot list <session-id>
```

Restore creates a new, independent session containing the state saved at the
snapshot. The original session remains running unless `--stop-source` is
supplied:

```sh
interceptor restore <source-session-id> \
  --checkpoint <checkpoint-id> \
  --provider-base-url http://127.0.0.1:19090/v1 \
  --allow-model reference-model
```

Stop, verify, and export a completed session:

```sh
interceptor stop <session-id>
interceptor logs verify <session-id>
interceptor logs export <session-id> --output ./evidence.tar
```

## Create a Blueprint from scratch

```sh
interceptor environment init ./my-environment
```

This creates `my-environment/environment.yaml` and refuses to overwrite an
existing file. Edit the placeholder image, routes, persistence declaration, and
review metadata; add any referenced assets below the same directory. Continue
with `environment validate`, `environment seal`, and `app build`.

See [Application integration](application-integration.md) before onboarding a
real target and [Environment Blueprint reference](environment-blueprint.md) for
the complete schema.
