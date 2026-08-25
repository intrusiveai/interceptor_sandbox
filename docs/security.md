# Security and trust model

Interceptor reduces an onboarded application's access to the host and network
while preserving a narrow model-provider path. It is designed for local
security testing, not multi-tenant remote execution.

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

The session disables general container networking, uses a read-only root,
limits resources, drops application capabilities, and mounts only declared
state. Synthetic hostnames resolve to loopback inside the container. Unknown
synthetic requests fail closed instead of escaping to a real service.

The only intended external request path is:

```text
application -> loopback model proxy -> protected bridge
            -> host connector -> fixed provider origin
```

The connector accepts one codec, one route family, one session identity,
monotonic request IDs, bounded bodies, and an explicit model allowlist. The
provider URL cannot come from the application request, credentials in the URL
are rejected, and redirects to a different origin are rejected.

## Credentials

The model API key is read from a host environment variable and retained by the
attached host connector. It is not passed in Docker argv, container environment,
session metadata, spool frames, logs, checkpoints, or exports.

All Blueprint credentials must be synthetic. Credential-like application
variables must be explicitly declared and tied to a synthetic service. Canary
values should also be synthetic and unique.

Avoid placing credentials directly on a shell command line. Use a dedicated
host environment variable and, if desired:

```sh
interceptor run ... --model-api-key-env MY_PROVIDER_API_KEY
```

## Local APIs

Application and cooperative control listeners bind to loopback and do not
authenticate. Do not expose them through `0.0.0.0`, SSH forwarding, a reverse
proxy, Docker port publication, or shared CI runners. Choose fixed ports only
when the local port ownership is understood; otherwise keep the default random
ports.

Operator-only endpoints remain behind protected bridge files/sockets and are not
routed by the loopback control server. Filesystem permissions are part of this
boundary, so keep the data root private and do not relax its modes.

## Transport selection

The regular-file spool is the default on all hosts. Its transient frames may
contain model/application data but never the provider credential. Private modes,
bounds, deadlines, cancellation markers, atomic rename, and orphan cleanup
reduce exposure and ambiguity.

Unix socket mode is an explicit optimization, not a stronger security mode.
Use it only when the host CLI, Docker daemon, and mounted session directory
share the same Linux kernel. Interceptor deliberately fails rather than falling
back or enabling container networking when the socket cannot be reached.

## Known limitations

- Container isolation shares the host kernel and is not equivalent to a VM.
- Same-user local processes can access loopback APIs and may read data permitted
  by host filesystem ownership.
- Evidence chains detect internal modification but are not externally signed.
- Sensitive model/application data is recorded unredacted in local evidence.
- Shared blobs are retained after session deletion.
- Docker Desktop and the Docker daemon are part of the trusted computing base.
- The MVP does not provide remote authentication, authorization, tenancy,
  encrypted evidence, secure deletion, or a secret manager integration.
- Snapshot restore covers declared logical state, not live process or kernel
  state.

Run Interceptor on a dedicated development machine or appropriately isolated CI
worker when the target image is not trusted. Keep Docker, the host OS, and the
Interceptor binary patched, and review every Environment asset before sealing.

