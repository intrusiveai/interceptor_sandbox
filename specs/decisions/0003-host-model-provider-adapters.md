# Decision 0003: Native application model APIs with host provider adapters

## Status

Accepted for the MVP. Supersedes the earlier Chat-only application seam in the
first version of this decision.

## Context

Every application model call must pass through Interceptor so the sandbox can
contain provider access, apply injections, and collect evidence. Real provider
credentials must remain on the host.

A single Chat Completions application contract appeared simple, but it moved
complexity into the wrong place. A Strands application using Bedrock Converse
would have to be rewritten to use Chat, or Interceptor would have to translate
between protocols whose state, tools, content, and streaming semantics do not
fully correspond. The same problem applies to applications built directly on
OpenAI Responses.

The security boundary is the mandatory proxy route, not a universal JSON
schema. Compatibility means allowing common application frameworks to keep
their native SDK protocol while preventing them from reaching the provider
directly.

## Decision

The in-container proxy recognizes these non-streaming native application APIs:

- OpenAI Chat Completions;
- OpenAI Responses and the corresponding Azure OpenAI routes;
- Anthropic Messages;
- Gemini Developer API and Vertex Gemini `generateContent`; and
- Amazon Bedrock Converse.

The host offers explicit, compatible adapters:

- `openai-chat`: Chat Completions pass-through to a fixed HTTP origin;
- `openai-responses`: Responses pass-through to a fixed HTTP origin;
- `bedrock-converse`: native Converse decoding followed by the AWS SDK for Go
  v2 `Converse` operation;
- `litellm`: Chat Completions or Responses pass-through to the matching LiteLLM
  gateway route;
- `anthropic-messages`: Messages pass-through to a fixed HTTP origin;
- `gemini-api`: Developer API `generateContent` at a fixed origin;
- `vertex-gemini`: `generateContent` constrained to a fixed project, location,
  and publisher; and
- `azure-openai`: supported OpenAI routes constrained to a fixed Azure
  resource and deployment allowlist.

The MVP uses like-for-like pairings and performs no cross-protocol model
translation. An incompatible application codec and host adapter fails
explicitly. Streaming and `ConverseStream` are outside the initial complete-
frame bridge.

The operator selects an exact model or deployment allowlist for every run or
restore. HTTP endpoints and authentication are host configuration. Production
credentials come from a supported host secret store or provider workload
identity; named environment variables are development-only. Bedrock uses the
host AWS SDK configuration and credential chain. No real provider credential
crosses the container boundary.

Application SDKs may require an authentication value before they will send a
request to a custom local endpoint. The Blueprint may therefore declare fake
OpenAI or AWS values accepted by the reserved `model-proxy` target. The local
proxy ignores these values and AWS signatures. They are never forwarded or
treated as host credentials.

Injection matching, delivery confirmation, model extraction, token accounting,
and evidence understand each native codec. Evidence records the final injected
application request, the provider request, and the native response. Bedrock
provider evidence is canonical SDK input/output JSON, not signed HTTP bytes.

## Consequences

Application owners configure an endpoint rather than rewrite their model
integration. Strands can retain Bedrock Converse; OpenAI applications can retain
Responses or Chat Completions; and gateway users can retain a LiteLLM-supported
OpenAI route.

The proxy contains a small codec-specific layer for routing, injections, and
evidence. This is more code than a Chat-only ingress, but substantially less
ambiguous than bidirectional provider translation and preserves native feature
meaning.

Initial Bedrock decoding deliberately supports the documented text/function-
tool subset and rejects unsupported native content. Responses bodies pass
through without being reduced to a Chat subset, though only the create route is
available. More native operations can be added explicitly without changing the
containment boundary.

Live provider behavior and provider-stored state are not part of a snapshot.
Reproducible restore comparisons must hold the provider, model, options, and
gateway routing constant or use a deterministic provider.
