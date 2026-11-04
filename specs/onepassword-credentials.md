# 1Password credential resolution

## Required behavior

Interceptor MUST resolve configured credentials from 1Password on the host using
the official 1Password Go SDK embedded in the product. Administrators MUST be
able to use desktop authentication for foreground development and a service
account for unattended execution.
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
| Profile `onepassword_account` | Required for desktop mode: the account name shown in the desktop app, or account UUID, passed to the SDK. Reject this field for service-account mode; the token selects its account. |
| Locator `backend_kind` | Must be `onepassword` and match the profile. |
| Locator `secret_reference` | Required bounded `op://vault/item/field` or `op://vault/item/section/field` reference. |
| Profile `allowed_locator_prefixes` | Require one or more complete vault, item, or section prefixes ending in `/`, such as `op://development/`. |
| Value `format` | Reuse `utf8` and `json-string-field`, with the existing `field` rules. |
| Time, size, cache settings | Reuse `timeout_seconds`, `max_value_bytes`, `max_cache_ttl_seconds`, and credential `cache_ttl_seconds`. |

The account selector MUST be nonempty, at most 256 UTF-8 bytes, and contain no
control characters; account names containing spaces MUST be accepted. References
and prefixes MUST be at most 2,048 UTF-8 bytes. They MUST have nonempty path components and no control characters,
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
    onepassword_account: "Development Team"
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

## Go SDK integration and authentication

Implementation MUST use `github.com/1password/onepassword-sdk-go` through the
existing `Backend.Read` and backend factory pattern in `internal/credentials`.
Pin one reviewed published SDK version in `go.mod` and `go.sum` in both products.
Extend `types.go`, `resolver.go`, `local_identity.go`, and `backends.go`; place the
SDK adapter and focused tests in `onepassword.go` and `onepassword_test.go`.
Use a small injectable client interface matching `Secrets().Resolve(ctx, ref)`
for controlled tests, following existing backend test patterns.

Create clients with `onepassword.NewClient` and explicit authentication options:

- Desktop: `WithDesktopAppIntegration(profile.OnePasswordAccount)`. The account
  value is the account name shown in the app, or its UUID, as supported by the
  pinned SDK. Administrators MUST enable the app's SDK integration. Approval
  remains in the 1Password UI; Interceptor MUST NOT collect account passwords.
- Service account: `WithServiceAccountToken(token)`, reading the token from
  `OP_SERVICE_ACCOUNT_TOKEN` in the host environment. Missing or empty bootstrap
  material MUST fail before a remote request. Service processes and CI MUST use
  this mode; desktop access is for interactive host sessions.
- Both: `WithIntegrationInfo` MUST identify the product and its actual build
  version. Use exactly one authentication option. Configuration MUST determine
  the mode; ambient CLI account, session, Connect or biometric settings MUST NOT
  select or override it. Do not mutate the process environment to configure SDK
  clients or pass credentials to guests.

Only resolve the configured field with `Secrets().Resolve`. SDK capabilities to
list, create, edit or delete items MUST NOT become product operations. Instantiate
the backend lazily so unrelated credentials and offline version/contract commands
work without an account, token or installed desktop app.

Pass the caller's bounded context through client creation and resolution. Apply
existing `max_value_bytes` and value validation immediately to returned UTF-8
bytes, without whitespace trimming. Reject any value returned with an error or
after deadline/cancellation. The SDK returns a complete string: this check limits
accepted secret size, not all memory allocated internally by the SDK. Tests and
documentation MUST describe that distinction accurately.

Run each uncached SDK lookup in a short-lived child of the existing product
executable, selected by the private `__onepassword-read` entry point. Pass bounded
authentication, account, reference and value-limit fields through stdin, and
return one bounded JSON value or allowlisted error code through stdout. Secret
values and bootstrap tokens MUST NOT be command arguments or persisted files.
The child MUST receive explicit SDK options and no ambient `OP_*` environment.
Discard unrestricted SDK stderr. The parent MUST bound output, terminate the
child process group on timeout/cancellation/overflow, and reap the child. An error
or late result MUST never disclose a partially returned value to a consumer.

The worker MUST initialize and read through the pinned SDK, then exit. It MUST
not run a shell, listen on a socket, remain resident, or require another installed
executable. Independent children MUST isolate accounts, tokens and SDK-global
state. The existing parent resolver remains responsible for cache policy and
required audit recording before a credential is given to a model or target.

## Cache, audit and diagnostics

Reuse resolver caching, TTL limits, invalidation, value selection and close
behavior. Failed reads MUST NOT populate the cache. Cache expiry/invalidation
MUST invoke SDK resolution again. Each uncached read MUST use the current bootstrap token and a fresh child
client without logging either token. A changed remote secret becomes visible on the next
uncached read subject to the provider's consistency behavior. Do not add
persistent secret storage or a second product cache.

`Secrets().Resolve` returns the value without an item version. Backend version
MUST remain absent; do not substitute a value digest or make extra item lookups.
Inspect SDK refresh/retry behavior and keep it within the same request deadline;
do not add product retries or claim one SDK call equals one network request.

Use bounded product-authored errors for missing bootstrap material, unavailable
desktop integration, deadline, cancellation, invalid/oversized value and SDK
rejection. Map recognized SDK error types to safe facts; never forward raw SDK
error strings, SDK debug logs, response bodies or arbitrary account/item details.
An opaque error MUST not invent a specific authentication/permission cause.
The existing redaction path MUST preserve recognized safe facts and context
cancellation identity. Reuse audit outcome/code conventions without introducing
a separate diagnostic framework.

Secrets, bootstrap tokens, account selectors, secret references, secret hashes
and raw SDK responses MUST NOT appear in campaign/session data, reports, exports,
guest inputs or diagnostics. Reuse audit failure rules; do not allow a dependent
request after a required audit write fails.

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

## Native compilation and release packaging

Build each host release on a matching GitHub-hosted OS and architecture:

| Platform | Runner |
| --- | --- |
| `linux/amd64` | `ubuntu-24.04` |
| `linux/arm64` | `ubuntu-24.04-arm` |
| `darwin/amd64` | `macos-15-intel` |
| `darwin/arm64` | `macos-15` |

The build script MUST reject a destination different from its running OS or
architecture. Use the runner's native C compiler with `CGO_ENABLED=1` so the
1Password desktop integration is present. Clear inherited target/compiler flags.
Use the pinned Go version, module checksums and native dependency cache. macOS
builds MUST use the runner's installed Xcode SDK with deployment target 15.0;
Linux builds use Ubuntu 24.04's libc baseline. Record the native platform,
compiler version, CGO setting and macOS SDK/deployment target in existing build
metadata or provenance. No additional SDK download configuration is required.

The SDK desktop adapter loads the IPC library installed by 1Password:
`libop_sdk_ipc_client.so` on Linux or `.dylib` on macOS. The app supplies this
runtime prerequisite; do not copy a developer's app library into releases.
Service-account authentication MUST work without the desktop app. Include SDK
license notices and dependencies in the existing SBOM where one is emitted.

Preserve archive names/layout, checksums, source and contract identities,
deterministic two-build comparisons, candidate qualification and promotion gates.
Execute each archived host binary on its matching runner and verify its version,
contract and CGO build identity. Inspect native dependencies and deployment
requirements. Keep existing packaged-runtime checks; do not add duplicate full
suite jobs or equate compilation with live 1Password account qualification.

Every archive MUST include a static `CGO_ENABLED=0` Linux companion for the
same architecture, version, source revision and integration contract. Build it
on the matching Linux runner. macOS packaging MUST obtain the checksum-verified
Linux archive produced by that same workflow and reuse its companion bytes,
rejecting mismatched source, version, platform or contract identity.

The host CLI MUST select the packaged companion for container construction on
both Linux and macOS. Validate its digest, ELF architecture and static Go build
identity before use. Never copy a CGO-enabled host CLI into an arbitrary app
image. Released CLIs MUST require no local source checkout or compiler. Keep
native Linux tests of both Linux and macOS packages' companions and existing
wrapper/runtime qualification.

## Verification

Both repositories MUST carry matching configuration fixtures (including both
schema namespaces) and rejection cases, independently of a sibling checkout.
Use an injected SDK factory/client for deterministic tests, plus the actual
SDK in build/runtime checks; a fake SDK alone cannot qualify native integration.

1. Test explicit desktop/service options and integration identity, missing token,
   account names containing spaces, ambient-mode interference, profile isolation,
   concurrent reads, process isolation and bootstrap changes on uncached reads.
2. Cover named/ID/section references, allowlist boundaries, invalid references,
   field conflicts, both value selections and exact byte preservation.
3. Cover successful reads, cache hits/expiry/invalidation, changed secrets,
   errors returning partial values, invalid UTF-8, empty/oversized results,
   canceled/deadline contexts, late results, bounded output and child reaping.
   Include a worker that ignores cancellation to verify parent-enforced termination.
4. Inject synthetic secrets into SDK errors/results. Verify their absence from
   logs, retained records, exports and safe diagnostics, including audit failure.
5. Exercise a representative existing command with private configuration, the
   production resolver, a controlled SDK client and a local model HTTP endpoint.
   Verify the endpoint receives the resolved credential, retained audit facts are
   correct, and a failed resolution prevents the dependent model request.
6. Build all four host destinations on their matching native runners. Verify
   actual build settings and dependency metadata, not just mocked compiler
   arguments. Keep existing repeat-build comparisons and run installed binaries
   from those archives in the existing native verification jobs, without rebuilding.
7. Add an offline SDK initialization/load smoke check to the native jobs. Verify
   service-mode code has no desktop library dependency and desktop support has not
   been compiled out. A missing app may produce the expected safe prerequisite
   error; that is not evidence of successful authentication. Unit tests MUST
   exercise SDK-specific errors without requiring an account or desktop app.

Run affected credential/configuration/authentication/command and release-script
tests, plus existing required checks. Existing opt-in credential checks SHOULD
qualify actual desktop approval (including locked/denied/canceled approval) and
service-account reads using a dedicated test item. Confirm actual cancellation,
account isolation and forced child cleanup for the pinned SDK. Record real platform/app/SDK
combinations separately from controlled tests. No test may require cloud writes,
paid model calls, or printing a secret without separate authorization.

## Documentation

Update `docs/credential-stores.md` and `docs/cloud-authentication.md`
with the common configuration and authentication selection. Document use through
the existing run and credential-check commands, including foreground and optional
service environments. Add a discoverable link from the existing specification
index and keep the README overview brief.

Instructions MUST cover enabling desktop SDK integration and selecting an account,
copying a secret reference, vault read permissions, service-token
bootstrap, private configuration, the existing credential selector, timeout and
cache behavior, and actionable safe troubleshooting. Include a desktop example
and its service-account variant. Explain that host access must be supplied to
the actual foreground/service process, and that approval time counts toward the
configured credential timeout. Do not suggest putting secrets in shell history
or enabling unrestricted SDK debug output to troubleshoot production credentials.

Update `.github/maintainers/CANDIDATE_RELEASES.md` with the native runner matrix,
compiler prerequisites, companion assembly where applicable, dependency checks
and native artifact verification. Document build prerequisites separately from
administrator runtime prerequisites. Installation MUST not require `op`.

Implementers MUST verify the pinned SDK and toolchain behavior against:
[official Go SDK](https://github.com/1Password/onepassword-sdk-go),
[SDK desktop option](https://github.com/1Password/onepassword-sdk-go/blob/main/client_builder_cgo.go),
[desktop library loading](https://github.com/1Password/onepassword-sdk-go/blob/main/internal/shared_lib_core.go),
[secret resolution](https://github.com/1Password/onepassword-sdk-go/blob/main/secrets.go),
[Go CGO](https://pkg.go.dev/cmd/cgo).
Commit the selected dependency versions and build inputs; these upstream `main`
links are implementation references, not release pins.
