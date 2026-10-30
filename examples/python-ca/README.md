# Python HTTPS without application trust changes

This example uses two Python virtual environments with HTTPX and Certifi. Both
sync and async clients retain `verify=True, trust_env=False`. Interceptor's build
installs its public CA in the declared Certifi bundles; the application supplies
no custom trust store, SSL context, or transport. Node behavior is unchanged.

Build the target image and prepare a writable copy of the Environment:

```sh
docker build -t interceptor-adapter-python:local examples/python-ca/python/target
cp -R examples/python-ca/python/environment /tmp/python-ca-environment
docker image inspect --format '{{.Id}}' interceptor-adapter-python:local
```

Replace the zero digest in `/tmp/python-ca-environment/environment.yaml` with
that image ID. The Blueprint declares `/app/.venv/bin/python` and
`/opt/worker-venv/bin/python`, plus the DNS name and HTTPS fixture. Then:

```sh
interceptor environment validate /tmp/python-ca-environment
interceptor environment seal /tmp/python-ca-environment
interceptor app build --environment /tmp/python-ca-environment --tag python-ca-wrapper:local
interceptor app verify --environment /tmp/python-ca-environment
interceptor run --environment /tmp/python-ca-environment \
  --model-provider litellm --provider-auth none \
  --provider-base-url http://127.0.0.1:9/v1 --allow-model unused
```

No model calls are made by this example. Through the [local API](../../docs/local-api.md),
invoke operation `ask` with JSON input `{"message":"hello"}`. Each turn retrieves
`https://web.sandbox.test/page` from both environments and returns the existing
stateful adapter response. Application logs report the client checks; Interceptor
evidence records eight fixture hits across two turns. An `error` message tests
application error handling. Stop the session with the normal session lifecycle
operation or interrupt `interceptor run`.

The original target image is untouched. Rebuild the target and wrapper after
upgrading dependencies. Certifi's upstream bundle is intentionally extended only
in the wrapper; private vendored stores and certificate pinning are not covered.

For automated installed-release qualification, build the existing local API
client and use a native archive from the same implementation revision:

```sh
go build -o /tmp/local-api-smoke ./examples/local-api-smoke
python3 scripts/qualify_release.py \
  --archive /path/to/interceptor_sandbox-v0.1.3-darwin_arm64.tar.gz \
  --version 0.1.3 --source COMMIT_SHA \
  --adapter-language python --python-ca --image python:3.13-slim \
  --client /tmp/local-api-smoke --output /tmp/python-ca-qualification
```

Select the archive for the host OS and architecture. The qualification CLI runs
outside the checkout with Go unavailable. It builds the wrapper, invokes the
application as an unprivileged user, checks evidence, verifies retained roots and
idempotence, proves rejection of untrusted and hostname-mismatched certificates,
and cleans up its own Docker resources. Preparing the target image can download
Python dependencies; CA injection and sandbox execution have no network access.
