# PostgreSQL wrapper qualification

The `python` (Psycopg pool + pgvector) and `node` (`pg` pool) targets are ordinary
applications. Their separately supplied adapters preserve conversation state.
Blueprint SQL creates populated related author/document tables, fixed vectors,
a counter and a refund table. Query `[1,0,0]` must retrieve document `one` joined
to author Alice. These illustrative vectors are supplied directly; a real agent
must generate document/query embeddings with the same model and dimensions.

For manual use, build either target image, inspect its image ID, and replace the
zero digest in that example's `environment/environment.yaml`. Run:

```sh
interceptor environment validate <environment-directory>
interceptor environment seal <environment-directory>
interceptor app build --environment <environment-directory> --tag support-wrapper:local
interceptor run --environment <environment-directory> <model-provider-options>
```

Use the `ask` operation with `{"message":"continue"}`, `change`, `refund`, or
`rollback`, or `clear_refunds`. Start with `--feedback-profile oracle-assisted`
to read the recorded refund oracle through live feedback between attempts.
A configured model must accept the example's `chat-fixture` identifier.
For an entirely offline deterministic model fixture, use the existing qualification
runner instead (it needs build-time network access for image dependencies):

```sh
go build -o /tmp/local-api-smoke ./examples/local-api-smoke
python3 scripts/qualify_release.py --archive <native-release-archive> \
  --version <version> --source <archive-source-commit> \
  --adapter-language python --postgresql --evidence-failure \
  --image python:3.12-slim --client /tmp/local-api-smoke --output <new-output-directory>
```

Pull the base image first. For Node, use `--adapter-language node` and
`--image node:22-bookworm-slim`. The runner extracts the archive outside the source
checkout, removes Go from the CLI's PATH, builds the wrapper, exercises native
SQL/TLS and both injections, reads live committed versus rolled-back refund
outcomes, restores database/application/injection state, checks a harness-authored
refund does not produce an application-success verdict, verifies earlier receipts
are unchanged, induces a capture failure, exports
evidence, and removes its test containers and images. Licenses are inspected in
the final image. Results identify the actual archive and host architecture.

To include native protocol regression checks, compile `internal/postgresql` with
`GOOS=linux GOARCH=<wrapper-architecture> CGO_ENABLED=0 go test -c -o /tmp/postgresql-test ./internal/postgresql`
and add `--database-test /tmp/postgresql-test`. These checks run inside the actual
wrapper and include cancellation at a full pool, independent transactions,
partial portal fetches, pipelining, typed rewrites and fresh-session isolation.

An optional runner exercises an unchanged local customer-support agent checkout:

```sh
python3 scripts/qualify_postgresql_agent.py --agent ../customer_support_agent_langchain \
  --archive <native-release-archive> --version <version> --source <archive-source-commit> \
  --image python:3.12-slim --client /tmp/local-api-smoke --output <new-output-directory>
```

It uses the agent's model/embedder hooks for deterministic fixtures and its normal
schema validation, database tools, document checks and refund transaction. It
does not modify the agent checkout or use agent-authored evidence for verdicts.

See the [database guide](../../docs/postgresql.md) for configuration, limits,
injection selectors, trust settings and compatibility details.
