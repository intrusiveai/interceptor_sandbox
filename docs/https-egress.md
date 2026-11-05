# Declared HTTPS egress

An HTTPS seam lets an application call a specifically approved external service,
including a tracing collector or guardrail API. The target container retains
`--network none`: its declared DNS name resolves to Interceptor's local TLS front
door, which forwards the exchange through the private bridge to a host connector.
The host contacts one configured HTTPS origin and verifies its certificate.

## Declare the application-facing names

Merge these settings into the Environment Blueprint:

```yaml
network:
  dns: {enabled: true}
  tls:
    enabled: true
    trust_mode: auto
    python_environments: [/usr/local/bin/python3]
  https_seams:
    - {id: tracing, hostname: tracing.sandbox.test}
    - {id: guardrail, hostname: guardrail.sandbox.test}
application:
  launch:
    environment:
      LANGFUSE_BASE_URL: https://tracing.sandbox.test
      GUARDRAIL_BASE_URL: https://guardrail.sandbox.test
credentials:
  - id: tracing-public
    environment_variable: LANGFUSE_PUBLIC_KEY
    value: pk-lf-synthetic
    accepted_by: tracing
  - id: tracing-secret
    environment_variable: LANGFUSE_SECRET_KEY
    value: sk-lf-synthetic
    accepted_by: tracing
```

Use the application's existing endpoint and logging configuration. No client
patch or disabled certificate verification is needed. Declare the Python
interpreters whose Certifi stores need the sandbox CA; Node retains its existing
CA environment configuration. Custom CA bundles and certificate pinning must be
configured to trust the sandbox certificate. The declared name can be the real
service hostname when preserving an existing URL is useful.

Seam wrappers protect supervisor mounts behind a private Linux directory, including
on Docker Desktop. They are bound to the host user ID; rebuild on another host
user account. This does not add a database engine to the image.

Names and IDs must be unique and cannot overlap simulated services or identity
endpoints. DNS and TLS are required. Build and verify the wrapper with the normal
`environment seal`, `app build`, and `app verify` commands after changing names.

## Configure host destinations and authentication

Create a host-only configuration file, for example `https-seams.yaml`:

```yaml
seams:
  - id: tracing
    origin: https://cloud.langfuse.com
    auth:
      kind: basic
      username_credential: langfuse-public-key
      password_credential: langfuse-secret-key
  - id: guardrail
    origin: https://guardrails.internal.example:8443
    ca_file: private-service-ca.pem
    auth:
      kind: header
      header: Authorization
      credential: guardrail-api-key
      prefix: 'Bearer '
```

Set each origin to the correct deployment for your account. Origins may be public,
private, or local to the host. `ca_file` is optional and resolved relative to this
configuration file; its PEM certificates supplement the host's system trust roots.
The origin cannot contain credentials, a path other than `/`, a query or fragment.
Application paths and queries are forwarded under that origin. Host DNS resolves
the origin, independently of the sandbox's loopback DNS answer.

Credential IDs reference the existing [host credential stores](credential-stores.md).
The file contains references, never real credentials. `auth.kind: none` supports
an explicitly unauthenticated service; `basic` requires two references, and
`header` accepts one credential-backed header and optional prefix. Application
Authorization, cookies, proxy credentials, `X-Api-Key`, and configured credential
headers are removed before bridging. Real authentication is added on the host.
An upstream response that echoes a managed credential is rejected.

Add these flags to the usual run command and model-provider options:

```sh
interceptor run --environment ./environment \
  --https-seam-config ./https-seams.yaml \
  --credential-config ./credential-stores.yaml \
  --model-provider openai-chat --model-credential-id model-api-key \
  --allow-model your-model
```

Every declared seam needs exactly one binding. Undeclared bindings, invalid trust
bundles, and credential resolution/audit failures prevent startup. The process
loads bindings and credentials at launch; restart to change them. Restores require
the same origins, credential references and private CA contents. Credential value
rotation behind an unchanged reference is allowed when starting a new process.

## Behavior and limits

The seam preserves ordinary HTTPS request/response bytes, status codes, binary
protobuf, gzip and repeated headers. It removes hop-by-hop and routing headers,
sets the correct upstream Host/SNI, and returns redirects without following them.
A redirected hostname still requires a declared seam. The connector does not retry
requests. An SDK may implement its own retries; a failed response does not establish
that the remote service performed no work.

The initial transport supports bounded exchanges, not gRPC, WebSockets, streaming,
CONNECT tunnels, cookie-based authentication, or mutual TLS. Message bodies use
the Blueprint's `limits.max_blob_bytes` bound. Exchange deadlines use the existing
application turn timeout, capped at the private bridge's ten-minute limit, including
background requests. Admission uses the existing private HTTP bridge concurrency
bound; overload returns a failure without queuing unlimited exchanges.
For credential-bearing responses and debug decoding, content
encoding must be identity or gzip; unsafe/uninspectable content fails closed.

Background SDK batching works without an active turn. During coordinated snapshots,
new seam calls are temporarily rejected while admitted exchanges drain. Pending
requests are never replayed after restore. External traces and service changes are
not rolled back. Application cleanup can flush SDK batches while the seam remains
available; Interceptor does not call SDK flush methods on the application's behalf.

Seams expose no injection surfaces and generate no model/service exchanges, turn
observations, oracle inputs or campaign feedback. Evaluate a guardrail through the
resulting target behavior and existing target oracles. The approved destination and
its service account define the application's external authority; this is not a
per-operation permission system for the remote service.

## Logging and troubleshooting

Ordinary connection logs contain time, session/seam ID, lifecycle and sanitized
failure category. On the host, inspect:

```text
<data-root>/sessions/<session-id>/logs/https-seam-connections.jsonl
<data-root>/sessions/<session-id>/logs/https-seam-local.jsonl
```

For explicit troubleshooting, enable debug for the affected seam in the host file
and restart the run:

```yaml
debug:
  enabled: true
  max_exchange_bytes: 65536
  max_total_bytes: 16777216
```

The defaults are 64 KiB of captured body content per exchange and 16 MiB of total
encoded diagnostic records per seam. The private host-accessible file is
`logs/https-seam-<id>-debug.jsonl`. Each line includes status, safe content metadata,
and `request_body` / `response_body` as base64 of decoded content, including upstream
error responses. `truncated` marks a partial capture; connection logs report a
storage limit or unavailable capture. Decode a body with `base64.b64decode` in
Python. Truncation affects diagnostics only, never the forwarded bytes.

Authorization and cookie headers are excluded. Known managed credential values
and query values are redacted before body truncation. This cannot identify every
application-specific secret: debug bodies may contain sensitive application data.
Diagnostic files are not campaign evidence and are excluded from snapshots and
standard evidence exports. Disable debug and restart when finished. Delete these
private diagnostic files after stopping the session, or use the normal session
purge workflow to remove all of its retained data.

If the SDK fails before issuing a request, there is no seam exchange to capture.
Inspect `logs/application.stderr.log` and `logs/application.stdout.log` in the same
session directory. SDK debug logging must be enabled through its own configuration;
the seam does not instrument the SDK. Application file logs can instead use a
Blueprint mutable root, but those files then have normal application-state
snapshot and export semantics.

## Qualification example

[`examples/https-egress/python`](../examples/https-egress/python) packages a normal
Python agent with a pinned Langfuse SDK and a guardrail HTTP client. Its invocation
adapter is installed by `app build`. The archive qualifier exercises startup and
shutdown trace flushing, allow/deny target behavior, verified TLS, host credential
replacement, undeclared-DNS blocking and debug on/off without Go in the CLI's PATH.

The automated endpoint is a local HTTPS fixture, including a Vault-shaped test
credential source. It is not a Langfuse deployment. For live QA, bind `tracing` to
your test Langfuse origin and real host-store references, run the target, and verify
`https-seam-startup`, `https-seam-guardrail` and `https-seam-shutdown` observations in
that deployment. Record the server/SDK versions and archive checksum; remove the
created test data afterward. SDK success or a fixture's HTTP 200 alone does not
prove ingestion by Langfuse.

The environment capability manifest adds `network.https_seams` with each seam's
ID and `maximum_message_bytes`. It exposes no host origin, credentials, trust
configuration or injection surface. This is informational: Interceptor manages
the seam without requiring Operator to configure or invoke it. The seam does not
change the Interceptor/Operator integration contract or require an Operator update.
