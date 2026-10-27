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
| Anthropic Messages | `POST /v1/messages` | `anthropic-messages` | fixed `/v1/messages` endpoint with pinned `anthropic-version` |
| Gemini generateContent | `POST /v1beta/models/{model}:generateContent` | `gemini-api` | constructed fixed-origin Developer API route |
| Gemini generateContent | `POST .../models/{model}:generateContent` | `vertex-gemini` | fixed project/location/publisher route with ADC or API key |
| OpenAI Chat or Responses | matching OpenAI route | `azure-openai` | fixed Azure resource `/openai/v1` route |
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

The initial native subset supports text, system instructions, bounded
generation parameters, structured text/JSON output, and client-side function
declarations, calls, and results. Unknown top-level native parameters and
streaming, multimodal/file input, background work, provider-stored conversation
state, and provider-hosted tools fail before provider contact.

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

Interceptor passes the supported Responses JSON through unchanged. Native
Items and response objects do not need to fit into a Chat Completions
representation. Provider-hosted tools, background work, and stored
conversation dependencies are rejected. Tool-result
injections understand `function_call` and `function_call_output` Items linked
by `call_id`. The matching `function_call` must be present in the request input.

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
routing, fallbacks, and provider credentials. For a trusted LiteLLM instance
with authentication disabled, use `--provider-auth none` and omit credential
options. This explicit no-auth mode is also available under `--production`.

Interceptor does not expose a native LiteLLM-specific application protocol or
Bedrock endpoint through this adapter. Use the OpenAI-compatible API that the
gateway publishes.

## Anthropic Messages

Point the Anthropic client at the local model proxy and use its normal
`POST /v1/messages` method. On the host:

```sh
interceptor run --environment ./environment \
  --model-provider anthropic-messages \
  --provider-base-url https://api.anthropic.com \
  --provider-api-version 2023-06-01 \
  --model-credential-id target-anthropic-key \
  --credential-config /etc/interceptor/credentials.yaml \
  --allow-model claude-approved
```

API-key mode uses host `x-api-key`; `--provider-auth workload-token` uses a
host bearer token. Application-supplied authorization and beta headers are not
forwarded. Tool-result injection links native `tool_use` and `tool_result`
blocks.

## Gemini Developer API and Vertex Gemini

Gemini Developer API uses a host `x-goog-api-key` and constructs
`models/{model}:generateContent` from the exact allowlist:

```sh
interceptor run --environment ./environment \
  --model-provider gemini-api \
  --provider-base-url https://generativelanguage.googleapis.com/v1beta \
  --model-credential-id target-gemini-key \
  --credential-config /etc/interceptor/credentials.yaml \
  --allow-model gemini-approved
```

Vertex uses `--provider-auth workload-identity` by default. Configure the
fixed base URL through project, location, and API version, ending before
`publishers/google/models`; Interceptor adds the publisher/model method and
uses Google application-default workload credentials on the host.

Native function injection links `functionCall` and `functionResponse` parts by
function name and ordinal. Grounding, code execution, cached content, files,
and multimodal parts are rejected.

## Azure OpenAI

Configure the fixed Azure resource base ending in `/openai/v1` and allow exact
deployment names through `--allow-model`. Stored API-key mode uses the host
`api-key` header. `--provider-auth workload-identity` instead uses Azure
workload identity or managed identity and acquires a bearer token on the host.
The application continues using native Chat Completions or Responses shapes;
no Azure/OpenAI-to-other-provider translation occurs.

## Credential source

For production, combine `--production`, `--model-credential-id`, and
`--credential-config`, or select a supported workload-identity mode. Explicit `--provider-auth api-key-env --model-api-key-env NAME` is also supported.
Implicit environment lookup remains a development compatibility source. See
[Host credential stores](credential-stores.md).

## Evidence, injections, and snapshots

Each exchange records the application codec, selected host adapter, model ID,
final injected request, provider request, native provider response, status,
normalized usage, finish/safety status, pinned non-secret API metadata, and
cryptographic fingerprints. With like-for-like routing, provider and
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


## Host authentication selectors

Use [the aligned authentication matrix](cloud-authentication.md) for both components.
`--provider-auth` accepts route-appropriate `api-key`, `secret-store`, `api-key-env`,
`workload-identity`, `aws-profile`, `aws-environment`, `google-adc`, `azure-cli`,
`azure-client-secret`, Anthropic `workload-token`, and LiteLLM `none`.
Named AWS profiles are supported in production. Azure CLI login and Google's
local ADC MUST be explicitly selected. Vertex API keys MUST use `X-Goog-Api-Key`;
ADC uses OAuth bearer tokens and quota-project attribution.

## Azure filtering evidence

The `model_exchange` journal event MUST preserve Azure response annotations in
`safety_status` without changing provider HTTP status, response bytes or returned
tool calls. The target application remains responsible for handling provider
filtering and deciding whether to execute its tools. These annotations MUST NOT
be treated as a target-resistance verdict by themselves.

- Chat: `safety_status.prompt_filter_results` MUST retain the native prompt array
  when present. `safety_status.choices` MUST contain each annotated choice's
  native `index` (when supplied) and `content_filter_results`, in response order.
  Prompt annotations MUST be recorded even if no choices are returned.
- Responses: when `content_filters` is present, `safety_status` MUST be an object
  containing that native value and any nonnull `incomplete_details`. Without
  `content_filters`, the existing `incomplete_details` projection MUST remain.
- The summary MUST preserve safe, detected-only, filtered and blocked annotations,
  including category details and Responses offsets. It MUST NOT infer an
  unreported filter outcome or discard unfamiliar annotation fields.
- `finish_status` MUST retain its existing first-choice finish reason (Chat) or
  native status (Responses). Absent Chat annotations MUST remain null.
- Provider errors MUST reach the application with their original status and body;
  full provider responses MUST remain available through retained response blobs.
  Error payloads that have no supported annotation location leave the summary
  absent rather than being interpreted as a successful or safe response.

Offline regression tests MUST cover native byte/status preservation, journal
annotations, retained blob identity, filtered tool-call responses, multiple Chat
choices, ordinary responses without Azure fields, and provider errors. These tests
MUST NOT claim live Azure qualification.

## Remote embeddings

Enable an independent OpenAI-compatible embeddings provider when the application
uses a remote embedding model. Generation keeps its existing provider, credentials,
and model allowlist; either provider can use the same endpoint if explicitly
configured that way. For example, add these options to your normal `interceptor run`
command:

```sh
--embeddings-provider openai-compatible \
--embeddings-base-url https://api.openai.com/v1 \
--embeddings-auth secret-store \
--embeddings-credential-config /absolute/private/credentials.yaml \
--embeddings-credential-id embeddings-key \
--allow-embedding-model text-embedding-3-small
```

`--allow-embedding-model` is repeatable and must contain at least one exact model
or deployment identifier. No generation credential or model permission is inherited.
The embeddings credential file uses the existing host credential-store format,
including AWS Secrets Manager. It can differ from the generation credential file;
this also allows cloud workload identity for generation alongside an embeddings key.

For an explicitly selected host environment key, use
`--embeddings-auth api-key-env --embeddings-api-key-env EMBEDDINGS_API_KEY`
in place of the stored-credential options. For an intentionally unauthenticated
gateway, select `--embeddings-auth none` without any credential options. Authentication
only defaults to `secret-store` when an embeddings credential ID is supplied.
Without the embeddings option group, embedding requests fail explicitly.

Configure the application's actual SDK clients to use
`http://127.0.0.1:18080/v1`, for both generation and embeddings, with their respective
model names and a declared synthetic credential (`accepted_by: model-proxy`). For
example, `OpenAIEmbeddings(base_url=..., api_key=..., model=..., chunk_size=128)`
configures LangChain. Host keys stay outside the container. There is no universal
embeddings environment variable: the application or its invocation adapter must
actually read the variables you supply. Startup embeddings work during adapter
initialization before the target becomes ready.

The supported operation is `POST /v1/embeddings`, with `model`, `input`, optional
positive integer `dimensions`, `encoding_format` (`float` or `base64`), and `user`.
Input can be nonempty text, a text batch, an integer-token array, or a batch of
integer-token arrays. Batches are limited to **128 inputs**; request and response
bodies are each limited to **4 MiB**, or the Blueprint's smaller blob limit. This
leaves room for evidence copies and base64 expansion inside the existing 32 MiB
bridge message. Reduce SDK chunk sizes or requested dimensions if a batch exceeds
these limits. Provider token limits still apply. Vector values and index associations
are preserved; malformed responses fail rather than producing substitute vectors.
Native Bedrock/Gemini embedding APIs and multimodal inputs are not supported by
this OpenAI-compatible route.

Upstream HTTP errors retain their bounded bodies, status, and retry headers.
Interceptor adds no retries; configure SDK retries/timeouts explicitly when needed.
Invalid or disabled routes and disallowed models do not contact an upstream.
Body, deadline, and evidence-storage failures produce explicit errors.

Protected `model_exchange` evidence uses codec `openai_embeddings`, records requested
and reported model names, input count, vector width, usage, and body references.
Embedding input tokens count once toward the session token budget. Initialization
calls without a turn ID remain session activity. Embedding calls neither consume
generation injections nor establish generation delivery, even if their input
contains an injected string.

See the [executable embeddings example](../examples/remote-embeddings/README.md)
for SDK dependency packaging and a wrapper qualification workflow. SDK tokenizers
must be packaged in the target image when needed; the network-isolated application
cannot download tokenizer data on first use. Interceptor itself does not require
a tokenizer or local embedding model.

Document and query vectors must use compatible models, dimensions, and preprocessing.
Switching an existing 384-dimensional corpus requires re-embedding its documents
and updating the application's vector schema as needed. Equal dimensions alone do
not make different models' vectors compatible. Database integration is independent
of the embeddings transport; this feature does not emulate PostgreSQL or pgvector.
