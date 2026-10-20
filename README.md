<h1><img src="assets/interceptor-sandbox.png" alt="Interceptor Sandbox icon" width="48" height="48" align="absmiddle"> Interceptor Sandbox</h1>

Interceptor Sandbox is an open-source environment for security testing AI
applications. It runs an application inside a Docker container with simulated
services, files, identities, and data that you control. You can introduce
prompt-injection content, observe how the application responds, and collect
evidence of its actions.

Use Interceptor directly through its CLI and local API, or connect
[Operator Sandbox](https://github.com/intrusiveai/operator_sandbox) to run adaptive
campaigns with [Attack Harness](https://github.com/intrusiveai/attack_harness).
Interceptor supplies the target environment, injection controls, snapshots, and
evidence; the tester or connected harness chooses the experiments.

## How it works

1. **Describe the environment.** An Environment Blueprint is a YAML file defining
   the application image, how to invoke it, its simulated dependencies, and the
   state and observations to retain.
2. **Build the sandbox.** Interceptor validates the blueprint and records file
   fingerprints, then builds a wrapper image containing your application and its
   sandbox supervisor.
3. **Run experiments.** The application runs without general network access.
   Supported model requests pass through a host-side connector, keeping real
   provider credentials outside the container. You control the simulated services
   and can inject content into supported tool results and environment data.
4. **Inspect and compare.** Interceptor records requests, responses, and state
   changes. Logical snapshots let you restore recorded state into a fresh session
   and compare experiments. Configured checks evaluate recorded behavior, while
   logs and evidence exports support further analysis.

Supported application interfaces include HTTP, command-line programs, stdin/stdout,
REPLs, Python, and Node. Model adapters support native non-streaming OpenAI Chat
and Responses, Azure OpenAI, Anthropic, Gemini/Vertex, Bedrock Converse, and LiteLLM
routes. See [application integration](docs/application-integration.md) and
[model providers](docs/model-providers.md) for the supported combinations.

## Getting started

You need a running local Docker installation and an application image matching
your CLI and Docker host's architecture. Use Docker Engine on Linux or Docker
Desktop on macOS. Extracted releases need **neither Go nor an Interceptor source
checkout**, including for wrapper builds on macOS. Keep the complete archive
together: the macOS CLI uses its adjacent `interceptor-linux` and `release.json`.
Archives use `interceptor_sandbox-<os>_<arch>.tar.gz` and extract into one
`interceptor_sandbox/` directory; add that directory to `PATH`.
See [release installation](docs/getting-started.md#install-a-release).

Interceptor is an MVP. The [local integration validation](docs/local-api-validation.md)
and [host-lifetime validation](docs/host-lifetime-validation.md) describe the tested
configurations and remaining qualification limits.

### 1. Build the CLI

For source development and the Go-based reference walkthrough below, install
**Go 1.26.5** and run from this repository:

```sh
make build
export PATH="$PWD/bin:$PATH"
```

The executable is `bin/interceptor`. The walkthrough below uses the bundled
reference agent and a local mock model provider; it needs no cloud account or
real API key.

### 2. Prepare the reference environment

Build the reference application and print its local image ID:

```sh
docker build -t interceptor-reference-agent:local examples/reference-agent
docker image inspect --format '{{.Id}}' interceptor-reference-agent:local
```

Edit `examples/reference-environment/environment.yaml`:

- Set `application.image` to `interceptor-reference-agent:local@sha256:...`,
  replacing the placeholder digest with the complete image ID printed above.
- Set `reviewed_by` and `reviewed_at` to your reviewer name and review time.

Validate the environment, seal its files, and build the sandbox wrapper:

```sh
interceptor environment validate examples/reference-environment
interceptor environment seal examples/reference-environment
interceptor app build \
  --environment examples/reference-environment \
  --tag interceptor-reference-sandbox:local
```

For your own application, start with `interceptor environment init ./my-environment`
and follow the [getting-started guide](docs/getting-started.md).

### 3. Start a session

In a separate terminal, from this repository, start the mock model provider:

```sh
go run ./examples/mock-provider
```

Then start Interceptor:

```sh
export INTERCEPTOR_MOCK_KEY='local-mock-key'
interceptor run \
  --environment examples/reference-environment \
  --model-provider openai-chat \
  --provider-base-url http://127.0.0.1:19090/v1 \
  --provider-auth api-key-env \
  --model-api-key-env INTERCEPTOR_MOCK_KEY \
  --allow-model reference-model
```

The key above is a synthetic value for the mock provider. For real model access,
configure a [secret store](docs/credential-stores.md),
[cloud identity](docs/cloud-authentication.md), or an explicitly selected host
API-key environment variable.

`run` stays in the foreground and prints the session ID. It also starts the local
Operator API at **`http://127.0.0.1:8080`**. One active instance is supported on a
trusted local host; no separate gateway process is required. Keep this terminal
open while testing. Optional service-manager execution is documented in
[host lifetime](docs/host-lifetime.md).

### 4. Exercise the target and inspect evidence

With the reference session running, open another terminal in this repository:

```sh
go run ./examples/local-api-smoke --output ./local-api-evidence
```

Use a new output directory for each run. This sample client attaches a test
campaign, invokes the application, creates and restores a snapshot, stops the
target, and exports evidence. It demonstrates the same local API used by Operator.
For your own client or campaign, see [Operator integration](docs/local-api.md).

Use the session ID from `run` to inspect retained data:

```sh
interceptor list
interceptor status SESSION_ID --json
interceptor snapshot list SESSION_ID
interceptor logs verify SESSION_ID
interceptor logs export SESSION_ID --output ./evidence.tar
```

A restore creates a new session ID; `interceptor list` shows retained sessions.
To stop an active target, run `interceptor stop SESSION_ID`. The API remains
available after target stop for evidence collection. Press **Ctrl+C** in the
`run` terminal to end the host service and shut down any remaining target.

## Evidence and cleanup

Each session keeps its own journals, artifacts, snapshots, and managed exports.
`INTERCEPTOR_DATA_DIR` selects a custom data root. Evidence stays available until
an administrator purges it; temporary transport files are cleaned up after
confirmed shutdown.

After ending the host service, use `interceptor purge --session SESSION_ID`,
`--campaign CAMPAIGN_ID`, or `--all` to remove retained data. Purging also removes
snapshots and the evidence needed for later evaluation. See
[file management](docs/file-management.md) for exclusions and recovery rules.

## Learn more

- [Documentation index](docs/README.md) and [CLI reference](docs/cli-reference.md)
- [Environment Blueprint reference](docs/environment-blueprint.md)
- [Injections and evaluation](docs/injections-and-evaluation.md)
- [Snapshots and restore](docs/snapshots-and-restore.md)
- [Model providers and filtering evidence](docs/model-providers.md)
- [Security model](docs/security.md)
- [MVP specification](specs/mvp.md) and
  [adaptive simulation contract](specs/external-adaptive-simulation.md)

## Development

Run the local checks from the repository root:

```sh
make test
make vet
```

Unit tests do not require Docker. Container integration and live provider
qualification require their corresponding runtime and credentials.

## License

Interceptor Sandbox is licensed under the [GNU AGPL 3.0](LICENSE).
