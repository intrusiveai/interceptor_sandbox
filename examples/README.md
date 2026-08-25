# Reference application and environment

The reference pair exercises the HTTP adapter, model proxy, stateful service,
attacker sink, both injection surfaces, and logical snapshot/restore flow.

Build the source image, replace the Blueprint's placeholder with its exact local
image ID, then validate, seal, and build the wrapper:

```sh
docker build -t interceptor-reference-agent:local examples/reference-agent
docker image inspect --format '{{.Id}}' interceptor-reference-agent:local
# Put the printed sha256 value after `@` in reference-environment/environment.yaml.
interceptor environment validate examples/reference-environment
interceptor environment seal examples/reference-environment
interceptor app build --environment examples/reference-environment --tag interceptor-reference-sandbox:local
```

The wrapper builder recognizes a declared digest matching a local tagged image
ID, verifies the match, and uses the local tag as its build base. Registry
`name@sha256:...` references continue to work normally.

For local end-to-end testing, start the deterministic mock model provider:

```sh
go run ./examples/mock-provider
```

The mock listens on `127.0.0.1:19090` and implements
`POST /v1/chat/completions` plus `GET /healthz`.

Runtime examples use the portable file-spool bridge by default. On a native
Linux Docker host, `interceptor run` and `interceptor restore` may opt into
bind-mounted Unix sockets with `--bridge-transport unix`. Do not use that mode
with Docker Desktop or a remote Docker daemon; there is deliberately no
automatic detection or fallback.
