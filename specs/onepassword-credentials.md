# 1Password credential resolution

## Required behavior

Interceptor MUST resolve configured credentials from 1Password on the host using
the installed `op` CLI. Administrators MUST be able to use desktop authentication
for foreground development and a service account for unattended execution.
Resolved values MUST flow through the existing credential resolver to existing
host-side consumers. Implement this as another credential backend, without a
new service, credential transport, or guest tool.

## Shared configuration

Operator and Interceptor MUST implement the same fields, defaults, validation,
and authentication semantics below. Both MUST continue accepting the existing
`operator.dev` and `interceptor.dev` credential/profile schema namespaces.
Existing configurations MUST retain their current behavior.

| Field | Required behavior |
| --- | --- |
| Profile `backend_kind` | Accept `onepassword`. |
| Profile `authentication` | Require explicit `onepassword-desktop` or `onepassword-service-account`. No inferred selection. |
| Profile `onepassword_account` | Required for desktop mode: an account ID or sign-in address passed as `--account`. Reject this field for service-account mode; the token selects its account. |
| Locator `backend_kind` | Must be `onepassword` and match the profile. |
| Locator `secret_reference` | Required bounded `op://vault/item/field` or `op://vault/item/section/field` reference. |
| Profile `allowed_locator_prefixes` | Require one or more complete vault, item, or section prefixes ending in `/`, such as `op://development/`. |
| Value `format` | Reuse `utf8` and `json-string-field`, with the existing `field` rules. |
| Time, size, cache settings | Reuse `timeout_seconds`, `max_value_bytes`, `max_cache_ttl_seconds`, and credential `cache_ttl_seconds`. |

The account selector MUST be nonempty, at most 256 UTF-8 bytes, and contain no
whitespace or control characters. References and prefixes MUST be at most 2,048
UTF-8 bytes. They MUST have nonempty path components and no control characters,
query, fragment, or `.`/`..` components. Names and IDs MUST both work, including
spaces in names. Treat components as literal 1Password names/IDs; do not apply
HTTP URL decoding, path cleaning, case folding, or template expansion. Reject
ambiguous/unsupported syntax rather than rewriting it. Match allowlists on
complete literal components, so `op://dev/` never admits `op://dev-other/`.

New fields MUST be rejected on unrelated backends. A 1Password locator MUST
reject fields belonging to another backend. Configuration errors MUST identify
the credential/profile and invalid field without printing the reference.

The common desktop example MUST be accepted by both products:

```yaml
profiles:
  - api_version: operator.dev/secret-store-profile/v1alpha1
    kind: SecretStoreProfile
    id: developer-secrets
    backend_kind: onepassword
    authentication: onepassword-desktop
    onepassword_account: example.1password.com
    allowed_locator_prefixes:
      - op://development/
    timeout_seconds: 60
    max_value_bytes: 65536
    max_cache_ttl_seconds: 300
credentials:
  - api_version: operator.dev/host-credential-ref/v1alpha1
    kind: HostCredentialRef
    credential_id: model-key
    store_profile_id: developer-secrets
    locator:
      backend_kind: onepassword
      secret_reference: op://development/openai/api-key
    value:
      format: utf8
    cache_ttl_seconds: 300
```

For unattended execution, administrators MUST change `authentication` to
`onepassword-service-account`, omit `onepassword_account`, and provide
`OP_SERVICE_ACCOUNT_TOKEN` to the host process independently. Tokens MUST NOT be
configuration-file values, command arguments, or references resolved recursively
through this backend. Administrators SHOULD grant only read access to the needed
vaults. The local prefix allowlist further limits configured reads; it does not
replace 1Password permissions.

## Host CLI and authentication

Implementation MUST use the existing `Backend.Read` and backend factory pattern
in `internal/credentials`. Extend `types.go`, `resolver.go`,
`local_identity.go`, and `backends.go`; put the bounded CLI adapter and focused
tests in `onepassword.go` and `onepassword_test.go` in that package.

The administrator MUST install 1Password CLI v2 with service-account support
(2.18.0 or later). Resolve `op` through the host's trusted executable PATH to an
absolute executable path. Interceptor MUST NOT install it or search a guest/workspace
for it. Missing CLI errors MUST explain the host prerequisite.

Reads MUST execute `op read --no-newline <secret_reference>` directly with an
argument array, never a shell. Desktop reads MUST add the explicit `--account`
selector. No secret values or tokens may appear in arguments. References remain
host-private configuration and MUST NOT be logged as command lines.

Each subprocess MUST receive a deliberately constructed environment. Strip
ambient `OP_*` values, permitting the administrator's `OP_CONFIG_DIR` for local
CLI configuration. Set `OP_DEBUG=false` and `OP_CACHE=false` so the product's
existing cache policy governs reads. Preserve the ordinary host environment
needed for desktop IPC, OS identity, TLS and network connectivity.

- Desktop mode MUST set `OP_BIOMETRIC_UNLOCK_ENABLED=true`, exclude service and
  session tokens, and use the administrator-enabled app integration. Approval
  happens in 1Password's UI under the caller's host identity. Interceptor MUST NOT
  collect account passwords or invoke `op signin`.
- Service-account mode MUST require a nonempty `OP_SERVICE_ACCOUNT_TOKEN`, copy
  it only into the credential subprocess environment, and set
  `OP_BIOMETRIC_UNLOCK_ENABLED=false`. It MUST NOT fall back to desktop/session
  authentication. An unattended service or CI job MUST use this mode.
- Both modes MUST exclude `OP_CONNECT_HOST`, `OP_CONNECT_TOKEN`, `OP_ACCOUNT`,
  and `OP_SESSION`/`OP_SESSION_*` inherited selectors. The selected mode MUST NOT
  be overridden by ambient configuration.

Subprocess stdin MUST be closed. Authentication approval and reads MUST respect
the resolver's deadline and caller cancellation. Products MUST terminate and
reap the credential subprocess on cancellation or output overflow; inherited
pipes MUST NOT make cleanup hang. No automatic retry is required.

Capture stdout only in memory, bounded to `max_value_bytes` plus one overflow
sentinel byte; bound any captured stderr to 4 KiB. Excess output MUST fail the
read and stop the child. A nonzero exit MUST discard even partially returned
stdout. Raw stderr, stdout, process arguments, and unrestricted subprocess errors
MUST NOT enter diagnostics. Preserve the value bytes exactly: use `--no-newline`
rather than trimming whitespace, then apply existing value validation.

## Cache, audit and diagnostics

Reuse resolver caching, TTL limits, invalidation, value selection, and close
behavior. Failed reads MUST NOT populate the cache. Cache expiry/invalidation
MUST cause a fresh CLI read; service tokens MUST be read from the selected host
environment for that invocation. Do not add persistent secret storage.

`op read` does not supply an item version in its value output. Backend version
MUST remain absent; do not substitute a value digest or perform additional item
lookups to manufacture one.

Use bounded product-authored errors for missing CLI, missing bootstrap token,
deadline, cancellation, output overflow, invalid value, and a nonzero CLI exit.
An opaque CLI rejection MUST say that authentication, permission or reference
resolution failed without claiming which unobserved condition occurred. The
redaction path MUST preserve only recognized safe error facts, never arbitrary
backend messages. Reuse existing audit outcome/code conventions and preserve
context cancellation identity. Do not introduce a separate error framework.

Secrets, bootstrap tokens, account selectors, secret references, secret hashes,
and raw CLI responses MUST NOT appear in campaign/session data, reports,
exports, guest inputs, or diagnostic output. Reuse existing audit failure rules;
do not allow a dependent request after a required audit write fails.

## Interceptor integration

Register the backend in the production resolver selected by
`--credential-config` and `--model-credential-id`. Existing declared HTTPS seam
credential references MUST use the same backend without a separate resolution
path. Verify a seam credential with a controlled endpoint as well as the model
workflow. Reuse the private credential-resolution audit and its existing failure
rules and retention/purge behavior. Credential checks MUST accept the new
backend through the existing `interceptor credentials check --live` command.

Credential profiles, references and resolution events MUST remain outside
CapabilityManifest and EvidenceBundle. This backend MUST NOT expand public
capabilities, the Operator integration contract, guest SDK authentication or
guest environment variables. Operator and Interceptor each resolve their own
credentials. No public contract identity change is required for this host-only
extension.

## Verification

Both repositories MUST carry matching configuration fixtures (including both
accepted schema namespaces) and the same configuration rejection cases. Tests
MUST run independently without requiring a sibling checkout or real account.

Using a controlled executable standing in for `op`, tests MUST cover:

1. Desktop and service-account argument/environment selection, ambient-mode
   interference, missing CLI/token, and desktop account selection.
2. Named and ID references, section fields, allowlist boundaries, malformed
   references, field conflicts, and value selection without whitespace changes.
3. Successful reads, cache hits, expiry, explicit invalidation, changed values,
   nonzero exits with partial stdout, invalid UTF-8, empty/oversized values,
   excessive stderr, cancellation and child/pipe cleanup.
4. Synthetic credentials and tokens in stdout/stderr/errors staying absent from
   logs, retained records and exports, including failure paths and audit failure.
5. A representative existing command workflow using a private credential file,
   the production resolver, and a controlled local model HTTP endpoint. The
   endpoint MUST verify the resolved credential was used; inspection of retained
   evidence MUST verify allowed audit facts and absence of secret material.
   A failed credential read MUST prevent the dependent model request.

Run affected credential, configuration, authentication and command integration
tests and existing repository-required checks. Real desktop approval and service
account reads SHOULD be qualified separately through the existing opt-in
credential checks using a dedicated test item. Controlled tests MUST NOT be
represented as live qualification. Tests MUST NOT require cloud writes, paid
model requests, or printing a secret.

## Documentation

Update `docs/credential-stores.md` and `docs/cloud-authentication.md`
with the common configuration and authentication selection. Document use through
the existing run and credential-check commands, including foreground and optional
service environments. Add a discoverable link from the existing specification
index and keep the README overview brief.

Instructions MUST cover CLI installation, desktop app integration and account
selection, copying a secret reference, vault read permissions, service-token
bootstrap, private configuration, the existing credential selector, timeout and
cache behavior, and actionable safe troubleshooting. Include a desktop example
and its service-account variant. Explain that host access must be supplied to
the actual foreground/service process, and that approval time counts toward the
configured credential timeout. Do not suggest putting secrets in shell history
or enabling CLI debug output to troubleshoot production credentials.

Implementers MUST verify command semantics against the official references:
[read](https://www.1password.dev/cli/reference/commands/read),
[authentication environment](https://www.1password.dev/cli/environment-variables),
[service accounts](https://www.1password.dev/service-accounts/use-with-1password-cli),
and [secret references](https://www.1password.dev/cli/secret-references).
