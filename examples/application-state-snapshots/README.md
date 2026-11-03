# Application state snapshots

These Python and Node targets keep conversation messages, session identity, and
an application safeguard in memory. Neither target implements checkpointing.
The separately supplied adapter exports and reconstructs that state. Each turn
calls Interceptor's model proxy; `change` also updates a simulated HTTP record and
an application file. Snapshots capture all three kinds of state together.

## Build and run

Use either `python` or `node` below. Docker must have a native Linux runtime image
(`python:3.12-slim` or `node:22-bookworm-slim`). The example has no package downloads.
Run from this directory with an installed Interceptor release:

```sh
language=python
docker build -t interceptor-state-example "$language/target"
cp -R "$language/environment" /tmp/interceptor-state-environment
image_id=$(docker image inspect --format '{{.Id}}' interceptor-state-example)
# In /tmp/interceptor-state-environment/environment.yaml, replace the placeholder
# application.image with interceptor-state-example@ followed by image_id.
interceptor environment validate /tmp/interceptor-state-environment
interceptor environment seal /tmp/interceptor-state-environment
interceptor app build --environment /tmp/interceptor-state-environment --tag interceptor-state-wrapper
interceptor run --environment /tmp/interceptor-state-environment \
  --model-provider openai-chat --provider-auth api-key-env \
  --model-api-key-env MODEL_API_KEY --provider-base-url "$MODEL_API_URL" \
  --allow-model chat-fixture
```

`MODEL_API_URL` must point to an OpenAI-compatible provider serving `chat-fixture`;
only the host needs its credentials. For real providers, choose an available model
in both the target's request and the host allowlist, then rebuild. The packaged
qualification below supplies a deterministic local provider instead.

Invoke the declared `ask` operation with `{"message":"change"}` twice, using the
[local API](../../docs/local-api.md) application invocation contract. Create a
snapshot with `interceptor snapshot create <session-id> --name before-change`.
Further `change` turns increment the service record and file. Restore through the
local API's `snapshot.restore` lifecycle operation, then send `continue`: the
agent remembers the saved conversation and logical session, and the record and
file have their saved values. No prior tool operations are replayed.

## Packaged qualification

From the repository root, build the qualification client and supply a native
release archive built from the implementation revision:

```sh
go build -o /tmp/interceptor-state-smoke ./examples/local-api-smoke
python3 scripts/qualify_release.py \
  --archive /absolute/path/to/interceptor_sandbox-v0.1.3-darwin_arm64.tar.gz \
  --version 0.1.3 --source <archive-source-commit> \
  --image python:3.12-slim --adapter-language python --state-snapshots \
  --client /tmp/interceptor-state-smoke --output /tmp/interceptor-python-state-results
```

Repeat with `--image node:22-bookworm-slim --adapter-language node` and a new output
directory. Use the archive matching the CLI host OS and architecture. The runner
extracts it outside the checkout, hides Go from the CLI, builds the target and
wrapper, exercises two restores, and removes its containers and images. It saves
verified evidence, actual model requests, image identities, and the result.

The checks compare model requests and application outputs with uninterrupted
continuations, including tool history, identity, safeguards, service records, and
files. They then run further turns to check that restored memory keeps updating.
The model provider is deterministic; no commercial model account is used.

## Adapter boundary

Only `environment/assets/adapter/` contains checkpoint integration. Python's
example uses synchronous hooks; Node's hooks are asynchronous. They return JSON
objects and reject incompatible state. The driver owns the state file and waits
for restoration before readiness. Keep `agent.json` separate from file-injection
namespaces and seeded assets. Application files such as `count.txt` can share its
mutable root without being part of the adapter's JSON payload.

These examples do not establish compatibility with an arbitrary framework or the
customer support agent in another repository. A real adapter must inventory its
application's messages, summaries, identity, guards, and object relationships,
and must not replay actions or change prompts to simulate restoration. Use one
invocation at a time; unsupported background state mutation prevents a reliable
snapshot even when method names are configured correctly.
