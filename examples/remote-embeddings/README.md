# Remote embeddings in a wrapper

The Python example calls a remote OpenAI-compatible embeddings endpoint with
pinned OpenAI and LangChain clients. Its invocation adapter is supplied separately
from the target image. Initialization embeds two documents; later calls embed a
query and invoke generation. The target image packages its tokenizer cache, so
runtime execution requires no external network or local embedding model. The
Blueprint declares the SDK-supported `DATA_GYM_CACHE_DIR` path explicitly because
application launch environments do not inherit arbitrary image environment values.

The example asserts deterministic fixture values. Use it through the qualification
command below; for your own application, replace those assertions with your
retrieval logic and choose compatible document/query model settings. The fixture
is a transport test, not a production embeddings model.

From a repository checkout with Docker and a downloaded CI release archive:

```sh
docker pull python:3.12-slim
go build -o bin/local-api-smoke ./examples/local-api-smoke
python3 scripts/qualify_release.py \
  --archive /absolute/path/interceptor_sandbox-v0.1.3-darwin_arm64.tar.gz \
  --version 0.1.3 --source FULL_ARCHIVE_SOURCE_COMMIT \
  --image python:3.12-slim --adapter-language python --embeddings \
  --client "$PWD/bin/local-api-smoke" --output dist/embeddings-qualification
```

Select the archive for your host. Qualification extracts it outside the checkout,
makes Go unavailable to the CLI, and uses the normal `environment validate`,
`environment seal`, `app build`, `app verify`, and `run` commands. It builds the
example image, starts two host fixture endpoints with separate keys, and invokes
the wrapper through the local API. It checks startup and turn vectors, both SDK
encodings, generation, denied models, rate limiting, cancellation, protected
evidence, and the exact packaged executable inside the wrapper. Images, containers,
and session resources created by the test are removed afterward; the selected
output directory retains results, logs, provider calls, and verified evidence.

Go is only needed to build the qualification client. A released Interceptor
installation does not need Go or source to build a wrapper. Dependency downloads
occur while building the target image, not during isolated application execution.

For a real application, keep its synthetic `OPENAI_API_KEY` declaration and local
SDK base URL in the Blueprint, then independently configure the real generation
and embeddings host endpoints, model allowlists, and credentials as described in
[Model providers](../../docs/model-providers.md#remote-embeddings). Initialization
and invocation timeouts should allow its bounded provider requests. Stateful
Python application memory and remote provider state are not snapshot-capable.
