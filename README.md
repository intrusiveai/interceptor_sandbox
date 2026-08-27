# Interceptor Sandbox

Interceptor Sandbox is a local-first harness for security testing deliberately
onboarded agentic applications in deterministic synthetic environments. It is a
single Go program with two execution modes: the host CLI and `sandboxd` inside
one application-specific container per session. It uses ordinary YAML, JSON,
JSONL, and content-addressed files—there is no database, Kubernetes deployment,
or remote control plane.

The implementation currently includes:

- strict Environment Blueprint validation and deterministic sealing;
- immutable wrapper-image construction from a digest-pinned application image;
- Docker containment with no network, a read-only root, dropped capabilities,
  bounded resources, and Docker Desktop-safe host relays;
- fixture, stateful JSON, simulated MCP, and attacker-sink HTTP services that
  fail closed;
- synthetic agent identities, delegated-user token exchange, protected local
  services, and injectable/restorable authorization state;
- adaptable HTTP, CLI command, stdin/stdout, REPL, Python, Node, and custom
  application invocation surfaces;
- a protocol-aware inference seam for native Chat Completions, OpenAI
  Responses, Amazon Bedrock Converse, and LiteLLM routes, with provider
  credentials kept outside the container;
- transit tool-result and persistent Environment Instance injections;
- hash-chained attack, control, and evaluation events plus a write-ahead state
  journal;
- logical snapshots made of filesystem deltas and blobs, and restore into a
  fresh session rather than a cloned filesystem; and
- deterministic oracle results, coverage reporting, integrity verification,
  and portable evidence exports.

## Build and verify

Go 1.26 or newer and a running Docker daemon are required for full
container operation. Unit tests do not require Docker.

```sh
make build
make test
make vet
```

The binary is written to `bin/interceptor` by `make build`.

## Basic workflow

```sh
interceptor environment init ./my-environment
# Edit environment.yaml and add reviewed synthetic assets.
interceptor environment validate ./my-environment
interceptor environment seal ./my-environment

interceptor app build \
  --environment ./my-environment \
  --tag my-agent-sandbox:local

export OPENAI_API_KEY='host-only-provider-key'
interceptor run \
  --environment ./my-environment \
  --model-provider openai-chat \
  --provider-base-url https://provider.example/v1 \
  --allow-model provider-model-id
```

`run` stays attached because that host process owns the provider connector and
the loopback bridges. Its printed application and cooperative-control URLs bind
only to `127.0.0.1`. These local endpoints intentionally have no authentication;
other processes running as the same OS user are inside the MVP trust boundary.
Provider credentials are read from the host environment and are never passed as
container arguments, environment variables, metadata, or recordings.

Administrative commands use the session's protected bridge transport:

```sh
interceptor status <session-id>
interceptor snapshot create <session-id> --name after-first-turn
interceptor evaluate <session-id>
interceptor coverage <session-id>
interceptor logs verify <session-id>
interceptor logs export <session-id> --output evidence.tar
interceptor restore <session-id> --checkpoint <checkpoint-id> \
  --model-provider openai-chat --provider-base-url https://provider.example/v1 \
  --allow-model provider-model-id
interceptor stop <session-id>
```

The portable default is `--bridge-transport spool`. Application, control, and
model calls cross the host/container boundary as bounded request/response files
published with atomic rename. The spool uses private directories, deadlines,
cancellation markers, deterministic serialized control ordering, bounded application
concurrency, and cleanup of expired artifacts.

Native Linux operators may explicitly select the faster bind-mounted Unix
socket path:

```sh
interceptor run ... --bridge-transport unix
interceptor restore ... --bridge-transport unix
```

`unix` is never selected automatically and has no fallback. It requires the
Docker daemon and CLI to share a Linux kernel with the bind-mounted session
directory. It is not supported by Docker Desktop for macOS, remote Docker
daemons, or filesystem-sharing layers that expose a socket pathname without
the originating kernel endpoint. A bad selection fails the session rather than
weakening containment or enabling container networking.

See [`examples/`](examples/) for a reference agent and Environment Blueprint.

## Documentation

The user documentation starts at [`docs/`](docs/README.md), including the
[getting-started guide](docs/getting-started.md), [Environment Blueprint
reference](docs/environment-blueprint.md), [local API](docs/api-reference.md),
[CLI reference](docs/cli-reference.md), [model providers](docs/model-providers.md),
the guide to [synthetic identity and delegated access](docs/identity.md), and a detailed guide to [logical
snapshots and restore](docs/snapshots-and-restore.md).

## Storage

`INTERCEPTOR_DATA_DIR` overrides the platform configuration directory. Session
metadata, logs, journals, checkpoints, generations, and shared SHA-256 blobs are
kept below that root. Deleting a stopped session retains shared blobs.

## Specifications

- [`specs/mvp.md`](specs/mvp.md) is the authoritative MVP specification.
- [`specs/decisions/0001-local-runtime-redesign.md`](specs/decisions/0001-local-runtime-redesign.md)
  records the move from the earlier distributed design.
