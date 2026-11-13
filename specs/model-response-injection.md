# Native model-response injection

## Outcome and scope

Interceptor MUST let a tester deterministically alter a model response before
it reaches the target application's model SDK or agent framework. This MUST
support testing unknown or malformed tool names, malformed tool arguments and
unexpected response structures without changing the target application's code.
The feature tests the target application, not Attack Harness or Operator.

The implementation MUST add `model_response` in `simulation` mode to the existing
injection mechanism. It MUST NOT add another proxy, provider, service, or injection
API. Existing model-tool-result injection MUST retain its opposite direction:
changing completed tool results before they are sent to a model.

The initial implementation MUST cover the currently supported non-streaming
JSON generation codecs: OpenAI Chat Completions, OpenAI Responses, Bedrock
Converse, Anthropic Messages and Google GenerateContent. It MUST operate on the
application-facing response after any host provider translation, using the
application's codec rather than the upstream provider's wire format.

Embeddings, streaming, invalid JSON bytes, HTTP status/header fault injection,
provider request mutation and provider-hosted tool execution are outside this
surface. Provider failures and non-JSON responses MUST retain existing handling.
A test MAY deliberately cause the target's SDK to reject an otherwise valid JSON
response; such rejection is a valid observation, not an Interceptor injection
validation failure.

## Selection and mutation

- Extend the existing definition and selector types with a required `model_codec`
  selector for this surface. It MUST accept only implemented generation codec
  identifiers. Existing `call_ordinal` MUST select `first`, `every` or a positive
  integer. No separate tool-name selector or tool registry is required.
- Reuse optional `request_body_pointer`, `request_body_equal` and
  `request_body_contain` predicates against the application request received by
  the model proxy, before other request mutations. Predicates MUST exclude
  host-added credentials and provider-only request details. An optional `turn_id`
  MUST match only supervisor-owned observed attribution, never target headers;
  requests without that attribution MUST NOT match a turn-specific definition.
- Reject selectors belonging to other surfaces, including service, collection,
  file, HTTP routing and tool-argument selectors. Arming MUST use existing
  validation and `invalid_injection` error patterns, attempt ownership and
  registered payload-digest checks. Unsupported codecs MUST fail at admission.
- Reuse `once` and `standing` lifetimes. `next_turn` MUST be rejected initially;
  no new turn lifecycle is necessary. Ordinals MUST count successful, parseable
  JSON responses matching the definition's codec and request predicates, in
  serialized response-processing order. Provider failures and nonmatches MUST
  NOT consume an ordinal or a once injection.
- The placement root MUST be the entire application-facing response JSON value.
  Reuse existing JSON Pointer and `replace`, `append_text`, `prepend_text`,
  `insert_array` and `merge_object` operations, including their existing type,
  index and allowed-field checks. Root replacement MUST allow a supplied complete
  response, including adding a tool call when the original response had none.
- Interceptor MUST allow a mutation that is valid JSON but intentionally violates
  the model response schema: unknown or syntactically invalid tool names, wrong
  argument types, malformed JSON embedded in an arguments string, missing fields,
  or incorrect envelope structure. It MUST NOT repair these values, consult a
  registered-tool allowlist, or validate the modified response as a normal model
  response before forwarding it. Transport and resource bounds still apply.
- Missing pointers and placement type errors MUST follow existing response
  injection semantics: record an unapplied result, leave that candidate unchanged,
  and consume a matching once injection. Oversized or unencodable output and
  persistence failures MUST instead fail closed through the model proxy's
  existing error handling; do not silently substitute a successful response.
- Eligible definitions MUST be fixed when the request is admitted to the model
  exchange so newly armed definitions do not affect already in-flight calls.
  Deletion/disabling before mutation MUST remain effective. Matching and state
  transitions MUST use existing serialization/journal mechanisms. Multiple
  eligible definitions MUST apply in the existing stable definition order;
  selectors MUST use the unchanged request context, placements the evolving body.
- Ordinal progress and consumed state MUST survive existing snapshot/restore
  and replay. Restoring a checkpoint MUST reproduce its armed state without
  adding model replay or a new checkpoint mechanism.

For example, for an OpenAI Chat Completions response containing a tool call:

```json
{
  "id": "malformed-tool-name",
  "surface": "model_response",
  "mode": "simulation",
  "scope": "once",
  "enabled": true,
  "selector": {
    "model_codec": "openai_chat_completions",
    "call_ordinal": "first"
  },
  "placement": {
    "operation": "replace",
    "pointer": "/choices/0/message/tool_calls/0/function/name"
  },
  "payload": "lookup_order("
}
```

The equivalent Bedrock Converse name pointer is
`/output/message/content/0/toolUse/name` when that block contains the selected
tool call. Documentation MUST explain that pointers depend on the actual native
response structure. Campaign requests MUST add the existing attempt and payload
bindings; the example above only illustrates the mutation definition.

## Proxy integration and evidence

- Reuse `internal/injection` for mutation, lifetimes and journal state, and
  `internal/bridge/proxy.go` for application-facing response interception.
  Preserve original provider response handling and extract usage, model identity,
  finish/safety metadata and budget accounting from the unmodified response.
  An injected usage field MUST NOT change actual usage accounting.
- Mutation MUST occur before any response body is written to the application.
  Preserve HTTP status and security-related headers; remove/recompute stale
  body-length or body-integrity headers when the body changes. With no applied
  mutation, preserve existing response bytes and behavior.
- Keep existing provider-response and model-response evidence references pointing
  to their original bytes. Store the final modified response separately and add
  its reference/digest to the existing exchange evidence. Record applied and
  failed placements, injection/attempt/payload bindings, codec and observed turn
  attribution through existing collectors. Do not log credentials.
- Record mutation and durable state before releasing injected bytes. A required
  blob, journal or evidence write failure MUST prevent release and use existing
  evidence-storage failure handling. Do not claim successful delivery on failure.
- Model-to-application injection MUST NOT use the tool-result path's confirmation
  of payload delivery to the model. Record the direction and distinguish an
  applied mutation from the result of writing the response to the application.
  A successful write proves neither SDK acceptance nor tool execution. Failed
  writes and any later evidence failure MUST retain the existing partial/unknown
  evidence semantics; already sent bytes cannot be retracted.
- Live feedback and exported evidence MUST identify this surface and direction
  while respecting existing feedback-profile visibility. A crash or invalid tool
  call MUST NOT be reported as successful code execution. Existing independent
  oracles and observed application behavior remain the basis for outcome claims.

## Capability manifest and shared contract

- Extend the existing capability manifest's injection surfaces/profiles to
  advertise `model_response`, its implemented codecs, scopes, selectors, native
  response root and placements. Reuse current typed discovery structures, adding
  only metadata needed to select this surface. Generated manifests MUST remain
  deterministic, secret-free and bound by their existing digest.
- Describe supported generation codecs as implementation capabilities, not a
  promise about the host's selected provider. Existing runtime codec/provider
  compatibility checks MUST remain authoritative; embeddings MUST never become
  eligible responses. This feature MUST NOT move provider configuration or policy
  into the shared contract or require a new runtime negotiation mechanism.
- Update the self-contained OpenAPI contract for the affected Operator
  interactions: `capabilities.read`, `injection.arm`, applicable feedback and
  evidence outputs, plus persisted definition/state schemas consumed in exports.
  Advance the shared contract version and deliberately update independent
  qualification version/document-hash pins. No concrete release identifier or
  digest is prescribed by this specification.
- Compatibility MUST continue to use the contract version and complete document
  hash, not an Interceptor source revision. Update schema/model drift tests and
  validate actual API exchanges, including invalid definitions and error replies.
- Existing release packaging/publication MUST retain the complete updated
  contract and its normal identity checks. No new release artifact or publication
  service is required. Operator's implementation changes belong to a separate
  task; this work MUST NOT edit the Operator repository.

## Documentation and verification

Update the injection guide, model-provider guide, API reference, capability and
contract maintenance guidance, and adaptive simulation specification. Explain
both traffic directions, native pointers, deliberate schema violations, evidence
limits and the need to rebuild wrappers for changed capability manifests.

Tests MUST cover:

- Every supported generation codec with a deterministic provider fixture,
  including native name and argument locations, and exclusion of embeddings,
  provider errors and streaming.
- Discovery through the native API followed by artifact/attempt registration,
  arming and application consumption. Exercise both existing control transports.
- Unknown/malformed tool names, wrong argument types and malformed embedded
  arguments JSON, insertion of a fake call, root replacement, and preservation
  of ordinary responses when no injection matches.
- Request predicates, observed turn matching, ordinal selection, once/standing
  behavior, multiple definitions, in-flight arming/deletion, placement failures
  and snapshot/restore of progress. Tests MUST prove deterministic state changes
  without assuming concurrent requests complete in request order.
- Original and delivered bytes/hashes, unchanged actual usage accounting,
  application-write failures, required evidence-storage failures, and visibility
  of injection outcomes in allowed feedback and exported archives.
- Rejection of invalid definitions, unsupported selectors/codecs and excessive
  output using existing API error envelopes; deliberate payload schema violations
  MUST reach the application rather than being rejected as invalid definitions.

A representative packaged-wrapper qualification MUST use an unchanged target
agent with a real model SDK/tool dispatcher and a local deterministic provider
fixture. Through the native campaign API it MUST arm a malformed-name injection,
invoke the target, and demonstrate that the SDK/dispatcher received the injected
name or rejected the delivered response. Repeat with malformed arguments and a
benign baseline. Retain original/modified response evidence and the observed
application result, verify required failure handling, and clean up test resources.
No live model subscription or actual code execution exploit is required. Retain
qualification logs outside the repository in the existing maintainer directory;
report platforms exercised and any unavailable packaged/CI validation explicitly.
