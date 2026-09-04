# Synthetic identity and delegated access

Interceptor can give an application a realistic local identity without placing
credentials for a real system in the sandbox. The application can retrieve a
short-lived credential, present it to a simulated HTTP or MCP service, receive
an authorization decision, and refresh or exchange the credential through its
normal authentication code.

This is useful for testing questions such as:

- Does the agent use its own permissions when it runs autonomously?
- Does it preserve the user's identity when acting on that user's behalf?
- Does it request only the scopes it needs?
- What happens when consent is removed, a grant is reduced, or a token expires?
- Can a prompt injection persuade the agent to attempt an operation its
  identity is not allowed to perform?

Interceptor does not try to test a real identity provider or a real service's
authorization implementation. Every identity, secret, key, and token described
on this page is synthetic and valid only inside its session.

## The identity model

The Blueprint separates four concepts:

| Concept | Meaning |
| --- | --- |
| Principal | A synthetic `agent`, `user`, or `service` identity. |
| Grant | Permission for one principal to access one simulated service. |
| Delegation | Permission for one agent to access one simulated service on behalf of one user. |
| Delivery | The standard mechanism an application uses to discover or retrieve its synthetic credential. |

An autonomous token identifies the agent as its subject. A delegated token
identifies the user as its subject and includes the agent as the actor. The
service therefore sees “Alice, acting through Support Agent,” rather than an
agent silently impersonating Alice. A grant or delegation must target a service
whose `authorization` uses the same issuer; validation rejects an identity
policy that would not actually be enforced.

## Complete Blueprint example

```yaml
services:
  - id: tickets
    kind: stateful_http
    listen: 127.0.0.1:18100
    hostnames: [tickets.sandbox.local]
    seed: assets/services/tickets.json
    routes: assets/services/tickets-routes.yaml
    authorization:
      issuer: sandbox-identity
      audience: http://tickets.sandbox.local:18100
      required_scopes: [tickets:read]
      allow_delegated: true

identity:
  issuers:
    - id: sandbox-identity
      kind: oidc
      listen: 127.0.0.1:18300
      hostnames: [identity.sandbox.local]
      issuer_url: http://identity.sandbox.local:18300
      access_token_ttl_seconds: 900

  principals:
    - id: support-agent
      kind: agent
      issuer: sandbox-identity
      client_id: support-agent-client
      client_secret: synthetic-support-agent-secret
      claims:
        name: Support Agent
    - id: alice
      kind: user
      issuer: sandbox-identity
      claims:
        name: Alice Example
        department: support

  grants:
    - id: agent-ticket-read
      principal: support-agent
      service: tickets
      scopes: [tickets:read]

  delegations:
    - id: alice-ticket-read
      agent: support-agent
      user: alice
      service: tickets
      scopes: [tickets:read]

  deliveries:
    - id: support-agent-oauth
      kind: oauth_client
      issuer: sandbox-identity
      principal: support-agent
      client_id_environment: TOOL_CLIENT_ID
      client_secret_environment: TOOL_CLIENT_SECRET
      token_url_environment: TOOL_TOKEN_URL

  inbound:
    issuer: sandbox-identity
    audience: support-agent
    application_header: Authorization
```

`client_secret` is deliberately synthetic. It is part of the reviewed and
sealed Blueprint and is not accepted by any external identity provider.

## Autonomous access

An `oauth_client` delivery adds the declared client ID, client secret, and token
URL variables to the application process. The application uses the ordinary
OAuth client-credentials grant:

```text
POST http://identity.sandbox.local:18300/token
Authorization: Basic <synthetic client credentials>
Content-Type: application/x-www-form-urlencoded

grant_type=client_credentials&
scope=tickets%3Aread&
resource=http%3A%2F%2Ftickets.sandbox.local%3A18100
```

The returned RS256-signed JWT identifies `support-agent`. Interceptor issues it
only when an active grant covers the requested resource and scopes. The ticket
service independently checks the signature, audience, scopes, and current
grant before handling the request.

If `scope` is omitted, the issuer returns the scopes available through matching
active grants. `resource` is recommended when an agent has access to more than
one service; `audience` is accepted as an equivalent MVP input.

## Delegated user access

For an HTTP application, the harness names a caller with
`X-Interceptor-Caller`. Interceptor replaces that harness-only header with a
synthetic bearer token in the application request:

```sh
curl -sS -X POST "$APPLICATION_URL/api/agent/invoke" \
  -H 'Content-Type: application/json' \
  -H 'X-Interceptor-Caller: alice' \
  --data '{"prompt":"summarize my ticket","ticket_id":"ticket-001"}'
```

The same capability is available in a cooperative turn using
`caller_principal`:

```json
{
  "operation": "invoke",
  "caller_principal": "alice",
  "media_type": "application/json",
  "input": "eyJwcm9tcHQiOiJoZWxsbyJ9"
}
```

The application can exchange the caller token using the OAuth token-exchange
grant:

```text
grant_type=urn:ietf:params:oauth:grant-type:token-exchange
subject_token=<synthetic Alice token>
scope=tickets:read
resource=http://tickets.sandbox.local:18100
```

The agent still authenticates to the token endpoint with its own client
credentials. The exchange succeeds only when the Blueprint contains an active
delegation for that exact user, agent, service, and requested scopes. The caller
token's audience must also name that agent, so a token intended for one agent
cannot be exchanged by another.

For a custom integration or a non-HTTP input format, create a caller token
explicitly and place it where the application expects it:

```sh
interceptor identity caller-token <session-id> \
  --principal alice \
  --issuer sandbox-identity \
  --audience support-agent
```

This command returns JSON containing the synthetic token and its public
metadata. The issuer and audience may be omitted when the Blueprint has an
HTTP `identity.inbound` declaration; otherwise both are required. Non-HTTP
adapters do not automatically decide where a caller token belongs in a command,
prompt, or function argument; that placement remains part of the declared
application interface.

## OAuth and discovery endpoints

Each `oidc` issuer provides:

| Endpoint | Purpose |
| --- | --- |
| `GET /.well-known/openid-configuration` | Issuer, token endpoint, JWKS, and supported grants. |
| `GET /.well-known/oauth-authorization-server` | OAuth authorization-server metadata. |
| `GET /jwks` | Current synthetic RSA public key. |
| `POST /token` | Client credentials and token exchange. |
| `GET /aws/container-credentials/{principal}` | AWS SDK-compatible synthetic credential retrieval when declared as a delivery. |

Access tokens default to 15 minutes and may be configured from 1 to 86,400
seconds. A new session, including a restored session, creates a new signing key.
Applications should use discovery and refresh credentials rather than storing a
token or public key indefinitely.

This is not a complete interactive OpenID Connect provider. It does not provide
login, ID tokens, browser authorization, PKCE, refresh tokens, device codes,
MFA, dynamic client registration, or token introspection.

## MCP authorization

Adding `authorization` to an `mcp_http` service protects its endpoint with the
same bearer-token checks. The service publishes OAuth Protected Resource
Metadata below `/.well-known/oauth-protected-resource` and returns a
`WWW-Authenticate` discovery hint when a token is absent or invalid.

This supports MCP clients that can use an already configured OAuth client or
client-credentials provider. Interceptor does not yet implement the interactive
authorization-code flow described by the complete MCP authorization
specification.

Authorization is service-wide in the MVP. Every protected request to that
service requires the service's `required_scopes`; different scopes for
individual HTTP routes or MCP tools are not yet supported.

## Other credential delivery methods

### Projected JWT file

```yaml
identity:
  deliveries:
    - id: support-workload-token
      kind: projected_jwt
      issuer: sandbox-identity
      principal: support-agent
      audience: local-workload
      path: /run/interceptor-session/identity/support-agent.jwt
      environment_variable: AGENT_IDENTITY_TOKEN_FILE
```

Interceptor writes the token atomically, refreshes it halfway through its
lifetime, and removes the transient file during a normal session stop. The path
must be beneath `/run/interceptor-session/identity/`.

This resembles Kubernetes and Azure projected workload tokens. It does not
emulate their external token-exchange endpoints or cloud control planes.

### AWS container credentials

```yaml
identity:
  deliveries:
    - id: support-aws-credentials
      kind: aws_container_credentials
      issuer: sandbox-identity
      principal: support-agent
      endpoint_environment: AWS_CONTAINER_CREDENTIALS_FULL_URI
      authorization_token_environment: AWS_CONTAINER_AUTHORIZATION_TOKEN
```

The standard AWS SDK credential chain calls the local endpoint and receives
synthetic `AccessKeyId`, `SecretAccessKey`, session token, and expiration
values. The generated endpoint URI uses the issuer's `127.0.0.1` listener so it
passes the SDKs' safeguards for plain-HTTP container credential endpoints. This
exercises automatic retrieval and refresh without static AWS variables.

The MVP does not implement STS, role assumption, IAM policy evaluation, or
general SigV4 authorization for simulated services. The existing Bedrock model
proxy can continue to accept synthetic AWS signing credentials while the host
uses its separate real AWS credential chain.

## Identity failure injections

Identity state is addressed through the existing `environment_state` fidelity
surface. Grants and delegations have an `active` field and a `scopes` array.
For example, this injection removes Alice's delegated consent:

```json
{
  "definition": {
    "id": "remove-alice-consent",
    "surface": "environment_state",
    "mode": "fidelity",
    "scope": "standing",
    "enabled": true,
    "selector": {
      "service": "sandbox-identity",
      "collection": "delegations",
      "key": "alice-ticket-read"
    },
    "placement": {
      "operation": "replace",
      "pointer": "/active"
    },
    "payload": false
  }
}
```

The issuer also exposes one managed fault record at service
`sandbox-identity`, collection `faults`, key `issuer`:

```json
{
  "unavailable": false,
  "reject_exchange": false,
  "issue_expired": false
}
```

Use an ordinary fidelity injection to replace one of those fields. Because
these are real issuer-state changes, the application encounters the failure
through its normal token or service request. Invalid mutations and changes to
immutable grant/delegation identifiers are rejected before the journal commit.

## Evidence and snapshots

Interceptor records token issuance, caller attachment, token exchange failure,
and service authorization allow/deny decisions. Events include subjects,
actors, audiences, scopes, and a SHA-256 token fingerprint. Raw bearer tokens,
client secrets, AWS secrets, and signing private keys are not recorded.

The state journal saves changes to grants, delegations, and issuer fault
settings. A snapshot records the corresponding journal position, so restore
reapplies consent removal, reduced scopes, revoked grants, and fault settings.

Access tokens and signing keys are deliberately not snapshotted. A restored
session creates a fresh signing key and issues fresh short-lived credentials
for the same restored logical identities. This avoids replaying an expired or
previously exposed bearer token.

## Current limitations

- Synthetic identities cannot authenticate to real external services, by
  design.
- Only client credentials and same-issuer user token exchange are implemented.
- Automatic caller-token attachment currently applies to HTTP application
  interfaces.
- Service authorization uses one scope policy for the entire service.
- Fixed cloud metadata IPs, Google STS, Azure token endpoints, AWS STS and
  AgentCore-specific APIs are not emulated.
- There is no live identity or general service proxy. Real service credentials
  never enter the sandbox.
