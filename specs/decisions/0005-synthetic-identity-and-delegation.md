# ADR 0005: Simulate identity and delegated authority inside the sandbox

## Status

Accepted for the MVP.

## Context

Agentic applications increasingly retrieve a workload identity before calling
tools and may act with authority delegated by a human user. Requiring a tester
to remove those flows would make Interceptor incompatible with the behavior
being tested. Passing through a real identity provider, however, would place
real credentials and network access inside the experiment and could allow the
agent to affect a real service.

The important security questions for Interceptor are whether the application
retrieves and refreshes credentials correctly, preserves the distinction
between agent and user, requests suitable scopes, handles revoked consent, and
respects authorization failures. Testing a production identity provider's own
authentication and policy engine is outside the product's purpose.

## Decision

Interceptor will simulate identity inside each Environment Instance.

- A Blueprint may declare local OAuth/OIDC-style issuers and synthetic `agent`,
  `user`, and `service` principals.
- Direct grants authorize one principal to a simulated service. Delegations
  authorize one agent to act for one user at one simulated service.
- The MVP issuer supports client credentials and same-issuer RFC 8693 token
  exchange. A delegated access token retains the user as subject and the agent
  as actor.
- Simulated HTTP and MCP services may require one service-wide audience and set
  of scopes. They verify both token cryptography and current Blueprint-managed
  grant/delegation state.
- HTTP harness requests may name a synthetic caller. Non-HTTP integrations may
  request a caller token and place it through their declared interface.
- OAuth environment settings, projected JWT files, and AWS SDK container
  credentials provide common application discovery mechanisms without live
  credentials.
- Grant, delegation, and issuer-fault changes use the existing managed-service
  journal and fidelity-injection machinery. Evidence records decisions and
  token fingerprints, never raw tokens or secrets.
- Signing keys and issued credentials are ephemeral. Snapshot restore preserves
  logical authorization state but creates new keys and credentials.

Interceptor will not proxy identity requests to production providers or
exchange synthetic identities for real external authority.

## Consequences

Applications can keep common workload and delegated-user authentication paths
while remaining inside a no-egress container. A tester can reproducibly model
consent removal, grant reduction, expiry, and issuer failure, then branch those
conditions through snapshots.

The implementation is intentionally not a complete identity platform. The MVP
does not include interactive authorization, PKCE, refresh tokens, device flow,
MFA, dynamic registration, production federation, cloud STS, SPIFFE/mTLS, or
route/tool-specific policy. Applications that require those exact protocols
need a future simulator extension or a small local adapter; they must not be
given a live identity path as a fallback.
