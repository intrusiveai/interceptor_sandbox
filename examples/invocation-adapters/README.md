# Python and Node invocation adapters

These examples retain a customer session across multiple invocations. The
`target/` build context contains only the application and dependencies; adapter
code lives separately in `environment/assets/adapter/`. Interceptor installs it
when constructing the wrapper. The examples make no model or database calls.

For Python, from this repository's root:

```sh
docker build -t interceptor-adapter-python:local examples/invocation-adapters/python/target
docker image inspect --format '{{.Id}}' interceptor-adapter-python:local
```

Replace the zero digest after `@` in
`python/environment/environment.yaml` with the printed image ID. Then:

```sh
interceptor environment validate examples/invocation-adapters/python/environment
interceptor environment seal examples/invocation-adapters/python/environment
interceptor app build --environment examples/invocation-adapters/python/environment --tag interceptor-adapter-python-wrapper:local
interceptor app verify --environment examples/invocation-adapters/python/environment
interceptor run --environment examples/invocation-adapters/python/environment \
  --model-provider litellm --provider-auth none \
  --provider-base-url http://127.0.0.1:9/v1 --allow-model unused
```

Invoke the declared `ask` operation with JSON input `{"message":"hello"}`
through the [local API](../../docs/local-api.md) printed by `run`. Its
`application.invoke` operation returns a turn whose output is available through
the observation API. The responses identify customer `alice` with turn `1`, then turn `2`.
Sending `{"message":"error"}` demonstrates an application exception. Sending
`{"message":"slow"}` exceeds the configured three-second turn timeout and
terminates that session's language process; start a new session before more calls.
Ctrl-C stops the session and invokes the adapter's cleanup method on normal exit.

The prebuilt `local-api-smoke` test client can perform these calls, check their
evidence, and stop the target automatically:

```sh
/path/to/local-api-smoke --adapter-only --output /tmp/adapter-example-evidence
```

Build that developer test client as shown below, or use an existing local API
client such as the testing harness. It is not an application runtime dependency.

For Node, use `node` instead of `python` in the image tags and example paths.
Its adapter imports an application file from `/app/support_app.mjs`, its own
helper by relative path, and a dependency from the target's `/app/node_modules`.
The Python example similarly imports the target application and its dependency,
plus a helper from the installed adapter directory. Neither requires an adapter
in the source application image. The release CLI requires no host language
toolchain; Docker images supply Python/Node. Keep macOS release files together
so `app build` can use the packaged Linux companion.

Use `interceptor list`, `interceptor stop <session-id>`, and
`interceptor purge --session <session-id>` to manage these sessions. Remove only the example
image tags you created with `docker image rm` when finished.

## Automated archive qualification

The repository qualification tool exercises both languages using the CLI extracted
from a native release archive outside the checkout, with Go absent from its PATH:

```sh
go build -o /tmp/interceptor-local-api-smoke ./examples/local-api-smoke
docker pull python:3.12-slim
python3 scripts/qualify_release.py \
  --archive /path/to/interceptor_sandbox-v0.1.3-darwin_arm64.tar.gz \
  --version 0.1.3 --source <archive-source-commit> \
  --adapter-language python --image python:3.12-slim \
  --client /tmp/interceptor-local-api-smoke --output /tmp/python-adapter-evidence
```

For Node use `--adapter-language node --image node:22-bookworm-slim` after pulling
that image, and a different new output directory. Select an archive matching the
host architecture and OS. The Docker daemon and runtime image must be Linux with
the same architecture. The qualification checks installed adapter/executable
bytes, repeated stateful invocation, errors, cleanup, and Interceptor evidence;
it removes its own containers/images/data. Go builds only the test client, not
anything needed by the released CLI. CI runs this same qualification against
downloaded Actions archives on Linux AMD64 and ARM64.
