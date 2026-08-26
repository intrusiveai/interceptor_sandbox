# Model providers

Interceptor is the application's only route to a model, but it does not force
every application to speak the same model API. The in-container proxy accepts
the native, non-streaming API used by the application. The attached host
process sends that same API shape to the selected provider.

This lets a Strands application continue using Bedrock Converse and an OpenAI
application continue using Responses or Chat Completions. The application must
change its endpoint configuration so calls go to Interceptor, but it does not
need to translate its prompts, tools, or responses to another provider's
format.

## Supported pairings

The MVP uses like-for-like routing. It does not translate between model APIs.

| Application API | Local route | Compatible `--model-provider` | Host destination |
| --- | --- | --- | --- |
| OpenAI Chat Completions | `POST /v1/chat/completions` | `openai-chat` | fixed `/v1/chat/completions` endpoint |
| OpenAI Responses | `POST /v1/responses` | `openai-responses` | fixed `/v1/responses` endpoint |
| Amazon Bedrock Converse | `POST /model/{modelId}/converse` | `bedrock-converse` | AWS SDK for Go v2 `Converse` call |
| OpenAI Chat Completions | `POST /v1/chat/completions` | `litellm` | LiteLLM `/v1/chat/completions` endpoint |
| OpenAI Responses | `POST /v1/responses` | `litellm` | LiteLLM `/v1/responses` endpoint |

An incompatible pairing fails explicitly. For example, a Responses request is
not converted to Bedrock Converse, and a Bedrock Converse request is not
converted to Chat Completions.

All requested model IDs must exactly match a repeated `--allow-model` value.
For HTTP providers, the application cannot choose the upstream server: the
operator supplies one fixed `--provider-base-url`, and cross-origin redirects
are rejected.

Streaming is not supported because the private host bridge exchanges complete,
size-bounded request and response frames. Omit `stream` or set it to `false`.
`ConverseStream` is not accepted.

## OpenAI Chat Completions

Configure an OpenAI-compatible SDK with:

```text
base URL: http://127.0.0.1:18080/v1
API key:  any declared synthetic value
```

The SDK then calls `POST /v1/chat/completions`. Interceptor ignores the
application's synthetic Authorization header and adds the real host key only
after the request leaves the container.

If the client requires a key, put a fake value such as
`OPENAI_API_KEY: interceptor-local` in `application.launch.environment` and
declare the same variable under `credentials` with `accepted_by: model-proxy`,
just like the Bedrock example below.

```sh
export OPENAI_API_KEY='host-only-key'

interceptor run \
  --environment ./my-environment \
  --model-provider openai-chat \
  --provider-base-url https://api.openai.com/v1 \
  --allow-model gpt-5.6
```

The non-streaming JSON request and response pass through without protocol
conversion. Matching model tool-result injections understand Chat
`tool_calls` and `tool` messages.

## OpenAI Responses

Use the same local base URL with the native Responses client method, which
calls `POST /v1/responses`:

```sh
export OPENAI_API_KEY='host-only-key'

interceptor run \
  --environment ./my-environment \
  --model-provider openai-responses \
  --provider-base-url https://api.openai.com/v1 \
  --allow-model gpt-5.6
```

Interceptor passes the Responses JSON through unchanged. Native Items,
Responses-only fields, hosted-tool definitions, and response objects therefore
do not need to fit into a Chat Completions representation. Tool-result
injections understand `function_call` and `function_call_output` Items linked
by `call_id`. The matching `function_call` must be present in the request input;
when only `previous_response_id` is supplied, Interceptor cannot recover the
tool name and arguments from provider-held state for selector matching.

The current bridge supports only `POST /responses`. Follow-up resource methods
such as retrieve, delete, cancel, WebSocket mode, background polling, and
streaming are not exposed. If an application relies on server-stored state,
remember that a sandbox snapshot does not snapshot the provider's state; for
repeatable restored tests, prefer an explicit transcript or otherwise keep the
provider state available.

## Amazon Bedrock Converse

An AWS SDK normally chooses an AWS endpoint and signs the request. Inside the
sandbox, configure it to use Interceptor's loopback model endpoint instead:

```yaml
application:
  launch:
    environment:
      AWS_ENDPOINT_URL_BEDROCK_RUNTIME: http://127.0.0.1:18080
      AWS_REGION: us-west-2
      AWS_ACCESS_KEY_ID: interceptor-local
      AWS_SECRET_ACCESS_KEY: interceptor-local-secret

credentials:
  - id: local-aws-access-key
    environment_variable: AWS_ACCESS_KEY_ID
    value: interceptor-local
    accepted_by: model-proxy
  - id: local-aws-secret-key
    environment_variable: AWS_SECRET_ACCESS_KEY
    value: interceptor-local-secret
    accepted_by: model-proxy
```

These are deliberately fake credentials. They satisfy SDKs that require a
credential before sending a signed request to the local endpoint. Interceptor
does not validate or forward that signature. Never put a real AWS key in the
Blueprint.

Some SDK versions or frameworks may need an explicit Bedrock Runtime
`endpoint_url` client option instead of the environment variable. Point it to
`http://127.0.0.1:18080`; the normal SDK-generated path remains
`/model/{modelId}/converse`.

The host side is configured separately:

```sh
export AWS_REGION=us-west-2
# Configure any AWS SDK-supported host credential source.

interceptor run \
  --environment ./my-environment \
  --model-provider bedrock-converse \
  --allow-model anthropic.claude-3-5-sonnet-20241022-v2:0
```

Interceptor extracts `modelId` from the local request path, checks the
allowlist, decodes the native Converse body into AWS SDK types, and calls
`Converse` with the host's AWS identity. The host SDK loads its standard
credential chain and handles signing, refresh, region, profiles, and workload
roles. The application never receives that identity.

The initial native Converse subset includes:

- text system and message content;
- function `toolUse` and text/JSON `toolResult` content;
- `toolConfig` with `toolSpec` and `auto`, `any`, or named tool choice;
- `inferenceConfig` (`maxTokens`, `temperature`, `topP`, and
  `stopSequences`);
- `additionalModelRequestFields`, `additionalModelResponseFieldPaths`, and
  `requestMetadata`; and
- text/tool-use responses, stop reason, token usage, and latency.

The MVP rejects unsupported native fields instead of discarding them. It does
not yet support `ConverseStream`, images, documents, video, guardrails, prompt
variables, service tiers, output/performance configuration, reasoning content,
or other specialized content blocks. Model-specific capability restrictions
still apply at Bedrock.

Tool-result injections understand native `toolUse`/`toolResult` blocks and
preserve the Converse request shape.

Provider or AWS SDK failures currently return Interceptor's bounded `502`
connector error rather than reconstructing every provider-specific exception
body. Successful Converse responses retain the native response shape.

## LiteLLM gateway

LiteLLM accepts both supported OpenAI-compatible routes:

```sh
export LITELLM_API_KEY='gateway-virtual-key'

interceptor run \
  --environment ./my-environment \
  --model-provider litellm \
  --provider-base-url http://127.0.0.1:4000/v1 \
  --model-api-key-env LITELLM_API_KEY \
  --allow-model my-gateway-alias
```

Use Chat Completions or Responses in the application and point its OpenAI base
URL to `http://127.0.0.1:18080/v1`. Interceptor passes the selected OpenAI API
shape to the matching LiteLLM route. LiteLLM owns downstream translation,
routing, fallbacks, and provider credentials. For a trusted local LiteLLM
instance with authentication disabled, omit `--model-api-key-env`.

Interceptor does not expose a native LiteLLM-specific application protocol or
Bedrock endpoint through this adapter. Use the OpenAI-compatible API that the
gateway publishes.

## Evidence, injections, and snapshots

Each exchange records the application codec, selected host adapter, model ID,
final injected request, provider request, native provider response, status,
usage, and cryptographic fingerprints. With like-for-like routing, provider and
application bodies normally match. Bedrock evidence is a canonical JSON view
of SDK input/output rather than the signed AWS wire request.

Authorization headers, AWS signatures, and host credentials are excluded.
Prompt, tool-result, and output bodies remain sensitive, so protect the local
Interceptor data directory.

A snapshot saves Interceptor and application state, not a live provider
account, a Responses object stored by OpenAI, a Bedrock model, or LiteLLM's
configuration. Keep provider choice, model, parameters, and gateway routing
fixed when comparing a source session with restored branches.

References: [official OpenAI Responses migration
guide](https://developers.openai.com/api/docs/guides/migrate-to-responses),
[Amazon Bedrock Converse API](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Converse.html),
[AWS service-specific endpoint
configuration](https://docs.aws.amazon.com/sdkref/latest/guide/feature-ss-endpoints.html),
[AWS SDK for Go v2 configuration](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html),
and [LiteLLM Responses API](https://docs.litellm.ai/docs/response_api).
