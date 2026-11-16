# Declared HTTPS egress seams

An application inside an Interceptor sandbox MUST be able to call a declared
external HTTPS service using its ordinary client. The service may provide tracing,
guardrails or another explicitly approved integration. Interceptor MUST forward
the real request and response while preserving the sandbox's general network
isolation. The seam MUST provide basic connection logging and explicit opt-in
request/response diagnostics for troubleshooting. Its traffic MUST remain outside
response injection and campaign observation/evidence collection.

The initial implementation MUST support public and private destinations, ordinary
bounded HTTPS request/response APIs, and OTLP over HTTP. Langfuse tracing MUST be
a qualification example. gRPC, WebSockets, streaming responses, arbitrary TCP
tunnels and inbound callbacks are outside this implementation.

## Configuration and trust boundary

- Add a `network.https_seams` list to the Environment Blueprint. Each entry MUST
  contain a stable seam ID and one exact application-facing DNS hostname. Empty,
  wildcard, ambiguous or duplicate declarations MUST be rejected, including
  collisions with simulated services, identity endpoints and other hostname
  routes. An empty list MUST preserve existing behavior.
- Declared seam names MUST resolve to the existing in-container HTTPS front door
  through closed DNS. DNS and TLS MUST be enabled for an environment using seams.
  Build-time certificates MUST include these names, and wrapper verification MUST
  bind them through the existing sealed-environment and TLS integrity mechanisms.
- Add a host-side `--https-seam-config FILE` option to `interceptor run`. This
  configuration MUST bind each declared seam ID to exactly one fixed HTTPS origin
  (scheme, hostname and port), authentication configuration and optional private
  trust roots. A configured origin MUST NOT contain user information, a query,
  fragment or a path other than `/`. Application paths and queries are forwarded
  under that origin. Undeclared or missing bindings MUST fail startup before
  target execution; configuration MUST NOT silently enable additional names.
- Real authentication MUST use host-managed credentials. Reuse the existing
  `--credential-config` resolver, stable credential IDs, secret-store backends,
  access restrictions, redaction and audit behavior. The seam configuration MUST
  contain references rather than plaintext secret values.
- Host bindings MUST be immutable for a running instance and its restores.
  The application, invocation input, HTTP headers, request body and injection
  configuration MUST NOT select or override an upstream origin, credential,
  trust root or host proxy setting.
- The container MUST retain `--network none`, the read-only root, existing user
  separation and supervisor-only bridge access. Unknown DNS names MUST remain
  blocked. Do not add Docker networking, a generic forward proxy, a separate
  gateway service or a new deployment dependency.

For example, an environment may declare:

```yaml
network:
  # Existing DNS and TLS configuration also applies.
  https_seams:
    - id: tracing
      hostname: tracing.sandbox.example
    - id: guardrails
      hostname: guardrails.sandbox.example
```

The corresponding host bindings select the actual public or private HTTPS
origins. The application uses the declared hostname through its normal SDK
endpoint configuration. Configuration MUST permit the declared hostname to match
the real service hostname when preserving an existing application URL is useful.

## Request path and destination controls

The required path is:

```text
application HTTPS request to a declared DNS name
  -> sandbox TLS front door
  -> supervisor-owned seam handler
  -> protected spool or Unix bridge
  -> host connector for that seam's fixed HTTPS origin
  -> actual external service
```

- Reuse `internal/simnet` hostname routing and TLS identity checks, and the
  existing private transport framing, cancellation, expiry and cleanup patterns
  in `internal/bridge`. Introduce a typed seam request/result where needed rather
  than passing external-service requests through model codecs or interpreting
  their bodies as model requests. Both supported bridge modes MUST work.
  Any transient spool payloads MUST stay supervisor-private, be cleaned up after
  completion/cancellation and be excluded from snapshots and retained evidence.
- The host MUST independently validate session identity, seam ID, method,
  request target, bounds and lifetime before dispatch. Only a sealed seam ID
  may select a host binding. Enforce applicable existing protections against
  stale or duplicate bridge requests; duplicates MUST NOT resend an external
  request automatically.
- Application-side TLS MUST retain certificate verification and matching SNI
  and Host checks. HTTP requests MUST NOT downgrade into an external HTTPS seam.
  Reject CONNECT, protocol upgrades and absolute-form or authority-form request
  targets. Build the upstream URL from the fixed origin plus the validated path
  and query; URL syntax or encoded delimiters MUST NOT change its authority.
- Upstream connections MUST validate the real server's hostname and certificate.
  Private services MUST support an explicitly configured host-side CA bundle.
  Do not use the sandbox CA as automatic upstream trust, disable verification,
  or infer trust from a failed handshake. Client certificate authentication is
  outside the initial authentication scope.
- The host connector MUST support both publicly routed destinations and explicitly
  configured private services, including a local HTTPS service. It MUST resolve
  the configured origin on the host, never using the sandbox's loopback answer.
  It MUST NOT derive a destination from forwarded headers or automatically use
  environment proxy settings. Redirects MUST be returned to the application
  without being followed by the connector; another hostname still needs its own
  declared seam. No destination may be learned from a redirect or response body.
- Forward method, path, query, status, ordinary end-to-end headers and body bytes
  faithfully. Preserve binary protobuf, JSON, content type, content encoding,
  gzip payloads and repeated headers. Strip hop-by-hop headers and regenerate
  transport framing and upstream Host/SNI. Do not silently decompress, normalize
  JSON, decode protobuf, or otherwise transform service payloads.
- Reuse existing request/response bounds, runtime deadlines and cancellation
  controls. Unsupported streaming or upgrades and oversized messages MUST fail
  explicitly; do not buffer indefinitely or truncate a response into apparent
  success. An upstream rejection or failure MUST reach the client with useful
  status and a sanitized connection failure when applicable. The connector MUST
  NOT add retries, fabricate guardrail approvals, or replace failures with
  successful mock replies.

These controls limit the outbound destination. They do not make every operation
offered by that external service safe or reversible. The configured service account
and approved origin determine the external authority available to the application.

## Host-managed authentication and application compatibility

- Support no authentication, HTTP Basic authentication using two credential
  references, and a credential-backed header with an optional literal prefix
  (for example, bearer tokens or `X-API-Key`). Reuse the existing resolver for
  each referenced value; do not add vendor-specific secret stores or secret
  interpolation templates.
- Remove application-provided Authorization, proxy credentials, cookies and configured
  credential-bearing headers before bridging them, and set the
  real upstream authentication only on the host. Application-supplied credentials
  MUST NOT override or supplement the selected host identity. Reject header names
  or values that permit header injection or alter routing and transport framing.
- Real credentials MUST NOT enter wrapper layers, container environment,
  Blueprint assets, bridge files, checkpoints, capability manifests, evidence or
  logs. Credential resolution and audit failures MUST fail the dependent request
  or startup; error messages MUST retain the existing redaction guarantees.
- If an SDK requires credentials to initialize, permit explicitly declared
  synthetic placeholders accepted by that seam. Extend the existing Blueprint
  credential validation accordingly. Langfuse's public-key/secret-key pair MUST
  be resolved on the host and converted into upstream Basic authentication;
  sandbox placeholders MUST never be used as the upstream identity.
- The application's existing integration code MUST remain usable through endpoint,
  synthetic credential and trust configuration. Reuse automatic CA installation
  and declared Python Certifi environments; preserve existing Node trust handling.
  Do not require users to disable TLS verification or patch their HTTP clients.
  Custom trust stores and certificate pinning that exclude the sandbox CA MUST
  fail clearly and be documented as requiring compatible trust configuration.

## Connection logging, debugging and lifecycle

- Default logging MUST be limited to basic connection lifecycle information:
  timestamp, session/seam identity, connection established/failed/closed and a
  sanitized failure category. Reuse existing operational logging and host log
  access. Do not create evidence payload blobs or campaign model/service exchange
  records for this seam, and do not feed its traffic into turn observations,
  campaign feedback, oracle inputs or injection delivery tracking.
- The seam MUST NOT participate in response injection. It MUST NOT advertise
  injection surfaces, register with generic service-response rewriting, or accept
  injection selectors targeting a seam. This restriction applies even if an
  external response resembles an existing simulated-service payload.
- Provide an explicit per-seam debugging option in the host configuration,
  disabled by default and controlled only by the host operator. When enabled, it
  MUST support bounded capture of request and response bodies, including upstream
  error bodies, so malformed payloads and service rejections can be diagnosed.
  It SHOULD include request identity, connection stage, timing, request method/path,
  resolved destination address, TLS validation outcome, HTTP status, content type
  and encoding. Binary and compressed bodies MUST remain inspectable with their
  encoding identified; do not misrepresent binary content as readable text.
- Debug diagnostics MUST be host-accessible without entering the container, stored
  privately using existing session storage/logging conventions, and excluded from
  ordinary evidence exports, snapshots, feedback, oracle inputs and injection
  delivery tracking. Document their location, explicit enable/disable procedure
  and cleanup. Diagnostic files may contain sensitive application data; capture
  MUST have configurable per-exchange and aggregate storage bounds. Reuse existing
  limits where applicable and clearly mark incomplete or truncated captures.
- Ordinary logs MUST NOT contain communication bodies, headers or query strings.
  Even in debug mode, authorization headers, proxy credentials, cookies, secret
  values and raw query values MUST NOT be logged. Capture only safe header
  metadata such as content type/encoding; redact known managed credential values
  from diagnostic content and errors. Obtain request diagnostics before host
  credentials are injected. Disable the affected capture if it cannot be produced
  safely, and report the diagnostic failure without dumping raw content.
- Debugging MUST NOT modify live request/response bytes, authentication, routing
  or guardrail decisions. Diagnostic truncation or a storage limit MUST NOT
  truncate the forwarded exchange. Client-supplied trace identifiers MUST NOT
  become trusted campaign or turn attribution. These restrictions MUST also
  cover front-door rejection, bridge-error and TLS error paths; existing service
  coverage logging MUST NOT become an alternate capture path for seam traffic.
- An SDK may fail before it contacts the seam. Debugging MUST distinguish that
  absence of observed traffic from an observed connection/request failure; it
  MUST NOT infer successful SDK initialization or delivery. Troubleshooting
  documentation MUST point users to existing host-retained application stdout
  and stderr logs for pre-request errors, as well as seam diagnostics for actual
  exchanges. Do not introduce application SDK instrumentation or a new log agent.
- Background SDK requests MUST function outside an active turn. Do not impose a
  turn/attempt-registration requirement on tracing export or attempt to correlate
  buffered traces with Interceptor turns. Connection logging does not establish
  receipt or persistence of telemetry in the external service.
- Connection errors MUST be returned to the client using the existing bounded
  transport error conventions, without claiming that a request which was already
  dispatched had no external effects. Do not retry such a request automatically.
  Diagnostic logging MUST NOT require the payload-evidence storage path; a debug
  logging failure MUST NOT fabricate an upstream outcome or trigger resending.
- External effects MUST be described as outside sandbox snapshot rollback.
  Snapshot, restore, stop and cancellation MUST coordinate admitted seam calls
  using the existing quiescence and lifetime controls. Do not persist or replay
  pending outbound bridge requests across a restored session. A restored
  application may make new requests with new request identities; remote state
  from previous requests remains external.
- Shutdown MUST keep the seam available while the existing application shutdown
  lifecycle permits SDK flushing, then drain or cancel admitted requests within
  configured limits. Interceptor MUST NOT inject calls to application SDK methods
  or claim that unsent in-process telemetry was delivered. Qualification examples
  MUST use the SDK's normal flush/shutdown behavior and verify receipt remotely.
- Guardrail evaluation remains an assessment of the target's behavior while its
  real guardrail integration functions normally. Existing target observations and
  oracles may assess that behavior. This feature MUST NOT use the guardrail
  communications themselves for campaign inspection or scoring. Opt-in debug
  capture is for troubleshooting the connection and integration only.

## Integration and documentation

- Extend Blueprint parsing, validation, sealing, hostname collection, wrapper CA
  generation and runtime verification using existing mechanisms. Add host binding
  loading, credential resolution and lifecycle handling in the existing CLI and
  host runtime, and seam routing in the supervisor with connection diagnostics
  accessible on the host.
- The seam MUST remain Interceptor-managed functionality and MUST NOT require
  Operator integration changes or an integration contract version bump. Follow
  the [contract scope guidance](../internal/integrationcontract/README.md).
  The environment capability manifest MUST NOT expose seam IDs, hostnames,
  limits, origins, credentials, trust configuration or controls. The seam MUST
  NOT add Operator operations, Harness tools, injection surfaces or feedback.
  Blueprint and host configuration remain private Interceptor inputs.
- Keep implementation in this repository; do not edit Operator or make
  Interceptor validation depend on its workflow.
- Update the architecture, security, network Blueprint, credential-store,
  application integration, troubleshooting and snapshot guides. Replace
  statements that only model traffic can leave the sandbox with the precise
  declared-seam rule.
  Include a configuration recipe for Langfuse tracing and an ordinary HTTPS
  guardrail API, public/private deployment guidance and explicit transport limits.

## Verification

- Add focused tests for declaration/binding validation, hostname conflicts, TLS
  trust and SNI/Host matching, origin preservation, redirect isolation, blocked
  undeclared destinations, authentication replacement, malformed bridge requests,
  cancellation, expiry and failures before and after dispatch.
- Verify that seam responses cannot be selected for injection and that neither
  mode produces payload-evidence blobs, campaign exchange records, turn
  observations or feedback entries. With debugging disabled, seam-only sentinel
  payloads MUST be absent from retained logs and exports. With debugging enabled,
  request/response sentinels MUST appear only in the private diagnostic capture;
  real credential sentinels MUST remain absent everywhere in diagnostics/exports.
- Debug tests MUST demonstrate useful host-visible request and error-response
  bodies for a malformed payload and an authentication rejection, plus connection
  diagnostics for invalid DNS and untrusted certificates. Test storage limits,
  clear truncation markers, redaction, cleanup and unchanged forwarded bytes.
  Include an SDK initialization failure that sends no request and demonstrate how
  existing application stderr identifies the cause without invented seam events.
- Verify unchanged protobuf and compressed payloads, repeated headers, query
  strings, response statuses, guardrail denials, private CA trust, concurrent
  requests and background batching. Verify real credentials are absent from
  container configuration, bridge frames, logs, snapshots and exported evidence.
- Exercise both private bridge modes and preserve the existing model connector,
  HTTP/MCP simulation, closed DNS, wrapper verification and snapshot regressions.
- Add repeatable qualification using an actual release archive and wrapper built
  through documented commands. With normal certificate verification enabled,
  demonstrate a target calling a host-reachable HTTPS test service, receive the
  real response, and verify that undeclared egress still fails. Exercise public
  and private destination configuration, including private CA verification.
- Use an installed, pinned Langfuse SDK to generate and flush a real trace through
  the seam; independently verify ingestion at a Langfuse deployment. Exercise an
  HTTPS guardrail test service with allow and deny responses during a target
  invocation, and verify the target's resulting behavior using existing target
  evidence. The seam itself MUST retain only connection logs and permitted debug
  diagnostics.
  An SDK-shaped HTTP fixture alone MUST NOT be presented as Langfuse qualification.
- Integrate automated qualification into the existing nonpublishing PR checks
  and candidate workflow using local fixtures where possible. Live service checks
  MUST use explicitly supplied test resources and credentials; unavailable live
  checks MUST be reported as validation gaps rather than replaced with claims
  based on mocks. Keep reports outside the source repository, identify the actual
  platforms and service/SDK versions exercised, and clean up created resources.

## Reference behavior

Langfuse documents OTLP/HTTP tracing at `/api/public/otel/v1/traces` and Basic
authentication with a public key and secret key in its
[API reference](https://api.reference.langfuse.com/api). Its SDKs support cloud and
self-hosted base URLs; see the
[SDK overview](https://langfuse.com/docs/observability/sdk/overview).
Background batching and explicit flushing are described in the
[SDK lifecycle guidance](https://langfuse.com/docs/observability/features/queuing-batching).
Use these as integration examples, not as vendor-specific logic in the seam.
