# Security and trust model

Interceptor reduces an onboarded application's access to the host and network
while preserving a narrow model-provider path. It is designed for local
security testing, not multi-tenant remote execution.

In practical terms, the application can interact with the model provider and
the fake world defined for the test, but it should not be able to browse the
internet, contact real business services, or read unrelated host files.

## Trust boundaries

Trusted in the MVP:

- the operator and harness;
- the `interceptor` host binary and its data root;
- the in-container supervisor and wrapper image construction;
- the Docker daemon and host kernel;
- the reviewed Sealed Environment; and
- other processes running as the same local OS user, because they can reach the
  unauthenticated loopback APIs or local data.

Potentially untrusted:

- the onboarded application and model-driven behavior;
- application/tool/model content;
- simulated attacker payloads; and
- data sent to declared attacker sinks.

The upstream model provider is an external recipient selected by the operator.
Prompts sent through the connector leave the local host under that provider's
terms.

## Containment properties

The session disables general container networking, makes most container files
read-only, limits resource use, removes unnecessary application privileges, and
shares only declared files with the container. Simulated service names resolve
only inside the container. If a simulated service does not understand a request,
it rejects it instead of forwarding it to a real service.

When closed DNS simulation is enabled, the container resolver points to
`127.0.0.1`. The resolver returns loopback only for declared names, has no
upstream, records bounded query evidence, and fails unknown names. A client that
hard-codes an external resolver is still blocked by `--network none`; Interceptor
does not add packet-redirection or firewall authority merely to disguise that
failure as a local DNS response.

Local TLS is termination by the simulated-service front door, not a proxy to a
real origin. A build-generated private CA signs a certificate limited to
declared hostnames. Only the public CA enters the wrapper image. The server
private key is stored under the private host data root and mounted read-only in
a supervisor-only directory. The CA signing key is discarded after issuance.
TLS requires version 1.2 or newer, and mismatched SNI/Host requests fail closed.

Dynamic attacker HTTP services are still sealed loopback destinations. Runtime
injection may change only a successful response body; it cannot add a listener,
route, redirect, upstream destination, or generic proxy behavior. Static
attacker sinks reject response injection.

The only intended external request path is:

```text
application -> loopback model proxy -> protected bridge
            -> host connector -> selected fixed provider or AWS Bedrock
```

The connector accepts only a native model API supported by the selected host
adapter, requests from the current session, bounded request bodies, and model
names explicitly allowed by the operator. The application cannot choose a
different HTTP destination or host AWS region. HTTP connectors reject redirects
to another server; the Bedrock connector uses only the endpoint resolution and
request signing performed by the host AWS SDK.

## Credentials

Local development may read an API key from a named host environment variable.
Production mode rejects that source. It resolves an approved stable credential
ID through AWS Secrets Manager, Azure Key Vault, Google Cloud Secret Manager,
or Vault KV v2, or uses direct Bedrock/Vertex/Azure workload identity. Values
remain in bounded host memory and are excluded from Docker configuration,
session metadata, bridge frames, logs, checkpoints, and exports. See [Host
credential stores](credential-stores.md).

All Blueprint credentials must be synthetic. Credential-like application
variables must be explicitly declared and tied to a synthetic service or the
local `model-proxy`. A Bedrock application may use fake AWS values solely so
its SDK will sign the local request; Interceptor discards that signature and
never substitutes it for the real host AWS identity. Canary values should also
be synthetic and unique.

Blueprint identity issuers use a new local signing key for every session. Their
OAuth client secrets, bearer tokens, projected JWTs, and AWS container
credentials are valid only in that session's simulated world. Interceptor does
not proxy identity requests to a real provider or exchange a synthetic token
for a real service credential. Raw tokens and private signing keys are excluded
from evidence and snapshots; projected token files are refreshed atomically and
removed during a normal stop.

Avoid placing credentials directly on a shell command line. For local
development only, use a dedicated host environment variable:

```sh
interceptor run ... --model-api-key-env MY_PROVIDER_API_KEY
```

## Local APIs

The integrated host API listens only on `127.0.0.1:8080`. Operator and
Interceptor share a trusted local host; no integration authentication or TLS is
used in the MVP. All local users able to access loopback are inside the trust
boundary. The harness has no network access and cannot call this listener.

Attachment binds one campaign, native operations enforce campaign access and record worker/revision attribution,
closure, expected revisions and budgets, and lifecycle restore rejects stale
sessions and stops the old target before replacement. No extra public cooperative
application/control ports are opened. Service failure is terminal, not a gateway
replacement event. Model-provider credentials retain their existing host-only
rules. See [Local integration](local-api.md).

## Transport selection

The regular-file spool is the default on all hosts. Its short-lived files may
contain model or application data but never the provider credential. Private
file permissions, size and time limits, cancellation handling, complete-file
publication, and crash cleanup reduce exposure and prevent partial requests from
being processed.

Unix socket mode is an explicit optimization, not a stronger security mode.
Use it only when the host CLI, Docker daemon, and mounted session directory
share the same Linux kernel. Interceptor deliberately fails rather than falling
back or enabling container networking when the socket cannot be reached.

## Known limitations

- Container isolation shares the host kernel and is not equivalent to a VM.
- Other processes running as the same local user can access local APIs and may
  read data permitted
  by host filesystem ownership.
- Evidence chains detect internal modification but are not externally signed.
- Sensitive model/application data is recorded unredacted in local evidence.
- Session evidence is retained until administrative purge; purge removes managed
  content but does not promise secure erasure of underlying media or external copies.
- Docker Desktop and the Docker daemon are part of the trusted computing base.
- The operations interface is single-installation and session scoped; it is not a
  multi-tenant control plane. Evidence encryption and secure deletion remain
  deployment responsibilities.
- Synthetic identity implements client credentials and same-issuer delegated
  token exchange, not interactive login, production federation, or live
  identity-provider proxying.
- Snapshot restore covers declared logical state, not live process or kernel
  state.
- Interceptor observes declared HTTP/MCP boundaries and model client-tool
  results; it does not interpose on arbitrary descendant process execution or
  private stdout pipes.

Run Interceptor on a dedicated development machine or appropriately isolated CI
worker when the target image is not trusted. Keep Docker, the host OS, and the
Interceptor binary patched, and review every Environment asset before sealing.
