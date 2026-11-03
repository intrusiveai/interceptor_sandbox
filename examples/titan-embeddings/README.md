# Titan embeddings in a wrapper

This example packages Boto3, LangChain's Bedrock embeddings client, and an OpenAI
client for independent generation. Its separately supplied invocation adapter
retains two document vectors created during initialization. Each turn embeds a
query, retrieves a document, exercises native binary and combined representations,
and invokes generation. The SDKs use local endpoints and synthetic credentials;
real provider credentials stay on the host.

## Qualification with a packaged CLI

With Docker and an archive matching your host:

```sh
docker pull python:3.12-slim
go build -o /tmp/interceptor-local-api-smoke ./examples/local-api-smoke
python3 scripts/qualify_release.py \
  --archive /absolute/path/to/interceptor-archive.tar.gz \
  --version "$ARCHIVE_VERSION" --source "$ARCHIVE_SOURCE_COMMIT" \
  --image python:3.12-slim --adapter-language python --titan \
  --client /tmp/interceptor-local-api-smoke \
  --output ../maintainers/interceptor_sandbox/titan-qualification
```

Use a new output directory. The qualifier extracts the archive outside the source
checkout, hides Go from the CLI, builds the target and wrapper, runs the normal
local API qualification, verifies evidence, and removes its temporary resources.
The host-only fixture verifies AWS request signatures and uses synthetic vectors
for deterministic retrieval. It also exercises native throttling/access errors,
independent credentials, all supported dimensions, and initialization/turn
accounting. It does not establish live AWS interoperability. CI runs this same
qualification against its downloaded release archive.

## Real-provider qualification

Use the extracted CLI for the commands below. Choose a host AWS identity with
Titan model access, and configure an independent real generation provider. The
application example uses an OpenAI-compatible generation client.

1. Build the target image with `docker build -t titan-example:local python/target`.
   Copy `python/environment` to a working directory. Replace its image placeholder
   with `titan-example:local@` followed by the digest from
   `docker image inspect --format '{{.Id}}' titan-example:local`.
2. Set `TITAN_FIXTURE_CHECKS` to `'0'` and `GENERATION_MODEL` to the permitted
   generation model in the copied Blueprint. Keep both local SDK endpoints and
   synthetic credentials. Remove any **host** fixture endpoint override; an
   application endpoint setting must still point at Interceptor.
3. Run `interceptor environment validate ENVIRONMENT`,
   `interceptor environment seal ENVIRONMENT`, and
   `interceptor app build --environment ENVIRONMENT --tag titan-wrapper:local`.
4. Start the wrapper with your generation options and the independent Titan
   options described in [Model providers](../../docs/model-providers.md#amazon-titan-text-embeddings-v2).
   For a named AWS profile:

   ```sh
   interceptor run --environment ENVIRONMENT \
     --model-provider openai-chat --provider-auth secret-store \
     --credential-config /absolute/private/credentials.yaml \
     --model-credential-id generation --allow-model YOUR_GENERATION_MODEL \
     --embeddings-provider bedrock-titan --embeddings-auth aws-profile \
     --embeddings-aws-profile embeddings --embeddings-aws-region us-west-2 \
     --allow-embedding-model amazon.titan-embed-text-v2:0
   ```

5. In another terminal with the same Interceptor data directory, run
   `/tmp/interceptor-local-api-smoke --adapter-only --output /absolute/new/results`.
   This invokes two turns, checks retained application state and an application
   error, stops the target, and exports verified evidence. Confirm successful
   Titan exchanges, dimensions/representations, generation, and observed-turn
   attribution in that evidence. Live vectors need not equal fixture vectors.
6. Stop the host `run` process. Use the normal session purge and application
   cleanup commands in [File management](../../docs/file-management.md) to remove
   the created session and wrapper records; remove the two uniquely named Docker
   images and your temporary environment copy after retaining required results.

Live qualification spends provider quota and needs a working identity, regional
model availability, and generation credentials. Record those prerequisites and
actual results separately from fixture qualification. No tokenizer, AWS credential
file, or external network access is required inside the application container.
