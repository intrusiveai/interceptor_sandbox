# 1Password credentials

Use the embedded 1Password Go SDK to read a configured vault field on the host.
No `op` command is required. The provider receives the selected value in memory;
configuration, campaign inputs, audit records and exports contain no secret value.

## Configure access

For interactive development, install and run the 1Password desktop app, sign in
and enable **Settings → Developer → Integrate with 1Password SDKs**. Use
`authentication: onepassword-desktop` and `onepassword_account` set to the account
name shown in the app or its UUID. Run the product as that desktop user and approve
the app's request when prompted. A background service without that user session
must use a service account instead.

For automation, create a 1Password service account with read access only to the
needed vaults/items. Supply its token as `OP_SERVICE_ACCOUNT_TOKEN` in the launching
process's environment through your runner's secret injection. Choose
`authentication: onepassword-service-account` and omit `onepassword_account`.
Do not put the token in the profile, command arguments, repository, logs or
campaign environment. A service-managed worker must receive it in its own private
service environment; it does not inherit a terminal's environment automatically.
There is no fallback between authentication modes.

Save this JSON as a private credential configuration file (mode 0600):

```json
{
  "profiles": [
    {
      "api_version": "operator.dev/secret-store-profile/v1alpha1",
      "kind": "SecretStoreProfile",
      "id": "developer-secrets",
      "backend_kind": "onepassword",
      "authentication": "onepassword-service-account",
      "allowed_locator_prefixes": [
        "op://development/"
      ],
      "timeout_seconds": 10,
      "max_value_bytes": 65536,
      "max_cache_ttl_seconds": 300
    }
  ],
  "credentials": [
    {
      "api_version": "operator.dev/host-credential-ref/v1alpha1",
      "kind": "HostCredentialRef",
      "credential_id": "model-key",
      "store_profile_id": "developer-secrets",
      "locator": {
        "backend_kind": "onepassword",
        "secret_reference": "op://development/openai/api-key"
      },
      "value": {
        "format": "utf8"
      },
      "cache_ttl_seconds": 300
    }
  ]
}
```

Replace the reference with your own vault, item, optional section, and field:
`op://vault/item/[section/]field`. Names and IDs are accepted; IDs avoid ambiguity
when items are renamed. Prefixes must end at a component boundary with `/`.
The sample grants the resolver access only beneath `op://development/`. 1Password
permissions remain authoritative; this local allowlist further restricts access.
References select a single field, not an attachment, template, command or recursive
reference. Use `value.format: json-string-field` with a named `field` only if the
selected field contains a JSON object whose string member is the credential.

## Limits and troubleshooting

An uncached lookup starts a short-lived child of the same executable. Its private
pipes carry the request/result, and its deadline terminates and reaps a stuck SDK
call. Neither SDK logs nor raw SDK errors are copied into evidence. The host keeps
selected values only in its bounded in-memory TTL cache. Expiry, explicit
invalidation or process restart reads again; a rejected refresh cannot reuse stale
credentials. A rotated token is picked up at the next uncached lookup.

`timeout_seconds` defaults to 10 (range 1–60), `max_value_bytes` defaults to 65536
(maximum 1048576), and `max_cache_ttl_seconds` defaults to 300. The credential TTL
cannot exceed its profile. Zero credential TTL disables caching. SDK response-size
checks bound accepted values, not the SDK's internal allocations.

A missing bootstrap token, unavailable desktop integration, rejected SDK read,
invalid value, canceled request or failed worker produces a safe error. For a
rejected read, check the account, vault permissions and exact field reference.
Do not enable unrestricted SDK debug logs or print credentials to diagnose it.
The product cannot distinguish every provider-side rejection because SDK errors
may include sensitive details.

The release embeds SDK v0.4.1. Desktop integration needs the app's matching native
IPC library; service-account mode does not require the app. Releases are built
natively on Ubuntu 24.04 and macOS 15, for AMD64 and ARM64. Native compilation and
offline missing-app checks do not establish live account access.

See the [official SDK setup](https://github.com/1Password/onepassword-sdk-go/tree/v0.4.1)
and [service-account permissions](https://developer.1password.com/docs/service-accounts/get-started/).

## Use with Interceptor

Pass `--credential-config /private/credentials.json` and
`--model-credential-id model-key` to `interceptor run`. The same reference can
supply the host credentials for a declared HTTPS seam; it is never installed in
the sandbox environment. [Credential stores](credential-stores.md) describes the
provider flags and host audit file.

To check one field explicitly without printing its value:

```sh
interceptor credentials check --live \
  --credential-config /private/credentials.json --credential-id model-key \
  --probe-id onepassword-read-01 --output /private/credential-check.jsonl
```

Use a harmless test field. A successful check establishes access for that profile
and field only; it does not qualify a model provider or every authentication mode.
