# Cross-VM gateway

The local control URL is convenient for same-user development; it is not the
production Operator boundary. The Cross-VM gateway exposes only typed
operations for one already-running session. It does not create sessions or
manage a pool of environments.

## Deployment

The gateway is an HTTP backend. In production, bind it only to a private
network interface and place it behind an enterprise-managed TLS load balancer
or private reverse proxy. The load balancer owns certificates and HTTPS; do
not publish the gateway's HTTP listener directly to an untrusted network.

Resolve the shared integration token from a supported host secret store:

```sh
interceptor gateway <session-id> \
  --listen 10.0.4.12:9080 \
  --auth-principal operator-production \
  --production \
  --auth-token-credential-id interceptor-operator-token \
  --credential-config /etc/interceptor/credentials.yaml
```

`--credential-config` supports AWS Secrets Manager, Azure Key Vault, Google
Cloud Secret Manager, and HashiCorp Vault through the common host credential
resolver. Retrieval uses workload identity or the configured Vault auto-auth
token sink and produces a secret-free resolution audit event.

For a developer workstation only, set a token of at least 32 bytes and omit
`--production`:

```sh
export INTERCEPTOR_GATEWAY_TOKEN='replace-with-a-random-development-token'
interceptor gateway <session-id> \
  --listen 127.0.0.1:9080 \
  --auth-principal local-operator
```

Environment token storage is rejected in production mode. Tokens must never be
passed as command-line values, placed in an Environment Blueprint, mounted
into the sandbox application, or logged.

## Two-token flow

The long-lived integration token is an administrative bootstrap credential,
not a run-operation credential. A trusted Operator Runtime or another trusted
harness uses it to issue, inspect, and revoke short-lived execution leases:

1. `POST /v1/execution-leases` with `Authorization: Bearer <integration-token>`.
2. Receive the public lease record and a randomly generated opaque
   `access_token`. The token is returned only in the issuance response.
3. Send typed operations to `POST /v1/cross-vm` with
   `Authorization: Bearer <lease-access-token>`.
4. Use `GET /v1/execution-leases/{lease_id}` for status and usage, or
   `DELETE /v1/execution-leases/{lease_id}` to revoke it. These two operations
   require the integration token.

The gateway hashes tokens before normal lookup and compares the integration
token in constant time. Lease state, usage, revocation, and idempotency state
are memory-resident and intentionally end when this per-session gateway
process exits.

An issuance request is strict JSON:

```json
{
  "api_version": "interceptor.dev/execution-lease-request/v1alpha1",
  "idempotency_key": "issue-campaign-01-revision-01",
  "operator_vm_instance_id": "opvm-random-per-boot",
  "campaign_id": "campaign-01",
  "session_id": "session-approved",
  "scenario_bundle_digest": "sha256:...",
  "capability_manifest_digest": "sha256:...",
  "environment_digest": "sha256:...",
  "application_digest": "sha256:...",
  "feedback_profile": "black-box",
  "allowed_operations": [
    "session.status",
    "capabilities.read",
    "attempt.register",
    "injection.arm",
    "injection.delete",
    "application.invoke",
    "observation.read",
    "snapshot.create",
    "session.stop"
  ],
  "limits": {
    "maximum_attempts": 120,
    "maximum_concurrent_invocations": 1,
    "maximum_artifact_bytes": 1073741824,
    "maximum_wall_seconds": 3600,
    "maximum_snapshot_creations": 8,
    "maximum_cumulative_snapshot_bytes": 4294967296
  },
  "expires_at": "2026-09-03T21:00:00Z"
}
```

Omitted or zero optional limit fields inherit the gateway's session-derived
ceiling. A requested value above that ceiling is rejected, never truncated.
The lease expiry cannot exceed the server's maximum TTL. The request must bind
the exact session, sealed Environment and Application, capability manifest,
feedback profile, Operator VM instance, campaign, and allowed operation set.
The optional ScenarioBundle digest provides correlation for AEF or another
planner but cannot broaden Interceptor policy.

Lease issuance requires an idempotency key. Repeating the exact request returns
the same lease and token; reusing the key with different bytes fails. The
Operator must protect the issuance response like any other bearer credential.

## Typed execution

The `POST /v1/cross-vm` body contains one strict `CrossVMRequest` and its typed
operation body. `lease_id` must identify the lease associated with the bearer
token. The gateway checks lease status and expiry, session and VM bindings,
operation allowance, deadline, body digest, expected revision, idempotency,
and lease budgets on every call.

Supported operations are `session.status`, `operation.status`,
`capabilities.read`, `artifact.register`, `artifact.read`, `attempt.register`,
`injection.arm`, `injection.delete`, `application.invoke`, `observation.read`,
`snapshot.create`, and `session.stop`. `injection.delete` accepts only an
`injection_id`; both injection mutations require the current session revision.
The gateway constructs the corresponding private local
request itself; caller URLs, paths, headers, commands, proxy destinations, and
filesystem locations are never forwarded.

`artifact.read` takes only a registered or ObservationView-referenced digest.
Its response wraps the exact bytes as base64 JSON `content`; it never accepts a
path or fetches a remote URL.

Mutation idempotency is scoped to lease, session, operation, body digest, and
key. A concurrent duplicate receives `operation_in_progress` and is never
executed twice; reuse with different content fails. Completed and unknown
receipts live in a bounded per-process replay cache. A result whose completion
cannot be proven is retained as `UNKNOWN`, preventing an automatic duplicate
mutation. Fresh mutations require the exact current session revision.

Snapshot count and cumulative canonical bytes are enforced. The gateway sends
the remaining allowance to Interceptor, which calculates candidate size and
rejects an excess before commit. Restore is not a live lease operation:
Operator revokes the lease, stops the guest, performs the trusted host restore,
and requests a new lease for the fresh session and VM revision.

Raw events, evaluation internals, evidence export, environment administration,
provider selection, Docker, host files, and unrestricted restore are not
available through this interface.
