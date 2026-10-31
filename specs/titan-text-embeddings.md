# Amazon Titan text embeddings

## Required outcome

Interceptor MUST allow a sandboxed application to obtain Amazon Titan text
embeddings through the native Amazon Bedrock Runtime API. Applications using
Boto3 or LangChain's Bedrock embeddings integration MUST work by configuring
their endpoint, region, model, and synthetic sandbox credentials, without changing
their embedding implementation or receiving real AWS credentials.

The supported model MUST be `amazon.titan-embed-text-v2:0`. Embeddings MUST work
during application initialization and subsequent invocations, independently of
the configured generation provider. Existing OpenAI-compatible embeddings and
Bedrock Converse generation MUST continue to work.

## Extend the existing implementation

The implementation MUST reuse:

- `internal/bridge/provider.go`, `connector.go`, and `proxy.go` for provider
  dispatch, model admission, the existing host bridge, and model evidence.
- `internal/bridge/embeddings.go` for reusable embeddings bounds and metadata
  validation, keeping native request and response validation distinct.
- `internal/bridge/provider_bedrock.go` and `internal/credentials` for AWS SDK
  client construction and explicit host authentication policies.
- `cmd/interceptor/embeddings.go`, `cmd/interceptor/main.go`, and
  `internal/hostrun/run.go` for independent embeddings configuration.
- Existing synthetic AWS credentials, observation attribution, token budgeting,
  recording, and required-evidence failure handling.

The implementation MUST NOT add a separate proxy service, model gateway,
tokenizer, runtime model download, or OpenAI-to-Titan translation layer. Generic
Bedrock InvokeModel access, other embedding families, streaming, and asynchronous
batch jobs are outside scope.

## Native request and response handling

1. Add an embeddings codec and provider for native
   `POST /model/{modelId}/invoke`. Routing MUST match the complete path and
   method, decode the model identifier consistently, and reject malformed or
   ambiguous paths. The connector MUST independently validate route, codec,
   model, provider compatibility, and the embeddings allowlist before AWS access.
2. Use the AWS SDK's Bedrock Runtime `InvokeModel` operation with JSON content
   and accept types. Do not send embeddings through Converse. Use an injectable
   client interface following the existing Bedrock provider pattern.
3. Validate `inputText` as a nonempty string; accept optional `dimensions` of
   256, 512, or 1024, optional Boolean `normalize`, and optional
   `embeddingTypes` selecting float, binary, or both. Omission MUST retain AWS
   defaults: 1024 dimensions, normalization enabled, and float embeddings.
   Reject invalid types, unsupported parameters, and invalid representation
   selections before contacting AWS. Do not change input text or normalize
   vectors locally.
4. Enforce the documented 50,000-character input ceiling using character rather
   than byte counting, alongside existing byte limits. Preserve provider
   rejection of inputs exceeding its token ceiling; do not estimate tokens as
   an admission guarantee or install a tokenizer to approximate that limit.
5. Each native request MUST contain one text input. Client libraries may issue
   multiple calls for document batches; Interceptor MUST NOT introduce a native
   array-input extension or silently split a request.
6. Preserve the native response body, including `embedding`,
   `inputTextTokenCount`, and `embeddingsByType`. Validate returned numeric
   values, token counts, requested dimensions, and representation consistency
   against AWS's documented encoding. Binary-only responses legitimately omit
   `embedding`; binary representations MUST NOT be interpreted as float vectors
   or OpenAI base64 vectors. Malformed or inconsistent responses MUST fail
   explicitly, without fabricated vectors or usage.
7. Preserve AWS request identifiers and SDK-consumable error types/statuses
   through an explicit bounded error mapping. Cover validation, denied access,
   throttling, unavailable service, and malformed upstream responses. Do not
   expose credentials or unfiltered SDK diagnostics. Cancellation and deadlines
   MUST reach the AWS call. Configure retries explicitly to preserve existing
   embeddings behavior: no hidden connector or SDK retries.
8. Apply the existing embeddings request/response byte ceiling, the smaller
   configured blob limit, and the complete bridge-frame limit to this codec.
   Enforce bounds before forwarding and before accepting returned evidence.

AWS's [request and response reference](https://docs.aws.amazon.com/bedrock/latest/userguide/model-parameters-titan-embed-text.html)
and [model limits](https://docs.aws.amazon.com/bedrock/latest/userguide/titan-embedding-models.html)
define the provider wire behavior. Tests MUST encode the supported shapes rather
than rely on network access to these references.

## Host configuration and sandbox boundary

- Add `--embeddings-provider bedrock-titan` and route it through the existing
  independent embeddings provider slot. Require the exact supported model in
  `--allow-embedding-model`; generation permissions MUST NOT authorize embeddings.
- Extend `--embeddings-auth` with the existing AWS authentication selectors:
  `workload-identity`, `aws-profile`, and `aws-environment`. Add independent
  `--embeddings-aws-region` and `--embeddings-aws-profile` settings. Reuse
  `AWSIdentityConfig` and its existing selection rules. Do not inherit generation
  authentication, region overrides, or profile selections. Ambient AWS settings
  may be used only as permitted by the explicitly selected authentication mode.
- Reject incompatible HTTP endpoint, bearer-key, stored-key, and unauthenticated
  options for this provider. Validate configuration before starting a session;
  missing identity, region, or model permissions MUST produce actionable errors.
- Keep AWS signing and credential refresh on the host. SDK clients inside the
  application MUST use the existing local model endpoint and declared synthetic
  AWS credentials. Guest authorization headers and guest endpoint/region choices
  MUST NOT select host credentials, grant model access, or redirect upstream
  requests. Real credentials MUST NOT enter images, Blueprints, evidence, or
  diagnostic output.
- Preserve the closed-network sandbox. SDK endpoint configuration MUST work
  without opening general AWS or Internet access from the application container.
  Support generation and embeddings together at the existing local endpoint,
  including Bedrock generation and a different generation provider.
- Keep provider configuration out of the environment capability manifest. No
  Interceptor–Operator compatibility-contract functionality is part of this work.

## Evidence, accounting, and injections

- Generalize checks that currently identify embeddings solely by the OpenAI
  embeddings codec. Both embeddings codecs MUST bypass generation injection
  rewriting, consumption, and delivery detection. An embedding of an injected
  string MUST NOT establish delivery to the generation model.
- Record Titan calls through existing protected model request, response,
  exchange, and error events. Include provider, native codec, operation,
  requested model, AWS request identifier when available, input count, effective
  dimensions, normalization setting, returned representation types, and usage.
  Do not invent a provider-reported model when the response contains none.
- Preserve original request/response bodies through existing integrity-covered
  blob references. Sensitive input and vector content MUST NOT become new public
  capability fields or unfiltered live-feedback content.
- Count `inputTextTokenCount` once toward the session token budget, including
  binary-only responses. Do not count output vector elements as tokens or count
  both float and binary representations separately.
- Reuse supervisor-owned observation attribution. Initialization and background
  calls MUST retain the existing session/ambiguous semantics; application-supplied
  headers MUST NOT establish turn identity.
- Required evidence failures MUST follow existing session failure behavior.
  Failure to durably preserve request evidence MUST prevent the AWS request;
  response/evidence failures MUST prevent successful delivery and subsequent
  turn admission as required by the existing storage-failure policy.

## Tests and complete workflow validation

The implementation MUST add focused tests covering:

- Native route/model parsing; defaults and explicit settings; Unicode input
  limits; malformed fields; disabled routes; wrong providers; missing or
  disallowed models; and absence of upstream calls on rejection.
- Float, binary-only, and combined responses; all supported dimensions;
  normalization on/off; malformed vectors and usage; byte/frame limits;
  cancellation; error mapping; and explicitly disabled retries.
- Independent AWS configuration and credentials, synthetic guest credentials,
  and concurrent generation/embeddings requests without permission crossover.
- Token accounting, observation windows, initialization calls, generation
  injection exclusion, evidence integrity, and injected storage failures.
- Existing OpenAI-compatible embeddings and Bedrock Converse regressions.

Extend the existing executable embeddings examples and qualification tools with
a native Titan example. Package Boto3 and LangChain's Bedrock embeddings client
in the target image; tests MUST use their actual SDK serialization and response
handling. Use a deterministic host-side Bedrock HTTP fixture with the real AWS
SDK transport for ordinary CI, configured through an explicitly host-controlled
test endpoint and synthetic host credentials. This MUST exercise InvokeModel
serialization and signing without a cloud account or guest-selected upstream.

The complete qualification MUST build and launch a wrapper from a packaged CLI,
embed multiple documents during initialization, embed a query during a later
turn, and perform a deterministic retrieval over the fixture vectors. It MUST
also issue a generation request, inspect native evidence and token accounting,
verify credential isolation, and clean up created resources. Document and query
embeddings MUST use the same model, dimensions, normalization, and representation.
This test MUST NOT introduce a database dependency solely for embeddings testing.

Extend the existing release qualification workflow to exercise this scenario
using its downloaded build archive. Preserve existing publication conditions;
qualification MUST NOT require cloud credentials or publish an artifact by itself.

A separately configured live AWS qualification MUST demonstrate successful
document and query embeddings and a generation call through a real wrapper.
Run it when an authorized AWS identity and model access are available; otherwise
report the missing live qualification explicitly. Fixture success MUST NOT be
reported as proof of real Bedrock interoperability. Keep local qualification
logs outside the repository under `../maintainers/interceptor_sandbox/` and retain
CI reports as workflow artifacts.

## Documentation

Update `docs/model-providers.md`, `docs/cloud-authentication.md`,
`docs/cli-reference.md`, and the embeddings example instructions to document:

- The native InvokeModel route, exact supported model, host embeddings options,
  and least-privilege `bedrock:InvokeModel` access to that model.
- Boto3 and LangChain endpoint/region configuration, synthetic guest credentials,
  and independent generation configuration, with runnable commands.
- Supported dimensions, normalization and representations, request limits,
  initialization behavior, evidence semantics, and failure diagnostics.
- The requirement to regenerate stored document vectors when changing embedding
  models or incompatible settings; equal vector widths do not establish
  compatibility. Existing seed vectors MUST NOT be silently regenerated.

Documentation MUST accurately distinguish native Titan support from the existing
OpenAI-compatible embeddings route and MUST NOT claim support for other Bedrock
models or operations.
