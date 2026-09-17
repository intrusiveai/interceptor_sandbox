# Host credential stores

Production model credentials can be resolved on the host without putting
plaintext secrets in an Environment Blueprint, CLI argument, container, or
application environment variable.

Supported built-in backends are AWS Secrets Manager, Azure Key Vault, Google
Cloud Secret Manager, and HashiCorp Vault KV v2. AWS, Azure, and Google use
their official Go clients and ambient instance/workload identity. Vault uses
the official client with a protected Vault Agent/Proxy token sink. The resolver
has read-only behavior and cannot list, create, change, rotate, or delete
secrets.

Create a permission-restricted host YAML file containing profiles and stable
credential references:

```yaml
profiles:
  - api_version: interceptor.dev/secret-store-profile/v1alpha1
    kind: SecretStoreProfile
    id: production-gcp
    backend_kind: gcp-secret-manager
    allowed_locator_prefixes:
      - gcp://projects/security-simulation-prod/secrets/interceptor-
    timeout_seconds: 10
    max_value_bytes: 65536
    max_cache_ttl_seconds: 300
credentials:
  - api_version: interceptor.dev/host-credential-ref/v1alpha1
    kind: HostCredentialRef
    credential_id: target-openai-key
    store_profile_id: production-gcp
    locator:
      backend_kind: gcp-secret-manager
      project: security-simulation-prod
      secret_name: interceptor-openai
      version: latest
    value:
      format: json-string-field
      field: api_key
    cache_ttl_seconds: 300
```

Use the stable binding:

```sh
interceptor run \
  --environment ./environment \
  --production \
  --model-provider openai-chat \
  --provider-base-url https://api.openai.com/v1 \
  --model-credential-id target-openai-key \
  --credential-config /etc/interceptor/credentials.yaml \
  --allow-model approved-model
```

Operator integration uses the same-host unauthenticated localhost API; it has
no integration token. This credential resolver applies to model providers.

Backend locator fields are strict:Backend locator fields are strict:

| Backend | Locator fields | Bootstrap |
| --- | --- | --- |
| `aws-secrets-manager` | `region`, `secret_id`, optional `version_id` or `version_stage` | AWS SDK instance/task/workload role chain |
| `azure-key-vault` | `vault_url`, `secret_name`, optional `version` | workload identity, then managed identity; no interactive credential |
| `gcp-secret-manager` | `project`, `secret_name`, `version` | application-default workload identity; CRC32C is verified |
| `hashicorp-vault` | `mount`, `path`, optional numeric `version` | fixed HTTPS origin and protected Agent/Proxy token file from the profile |

Profiles constrain exact regions/origins and locator prefixes, request timeout,
value size, and cache TTL. Values may be a bounded UTF-8 string or one named
top-level JSON string field. Vault requires an explicit JSON field. Binary
values, NUL, templates, commands, recursive references, and caller-selected
locators are rejected. Errors are centrally redacted.

Each resolution appends a permission-restricted host audit record under
`$INTERCEPTOR_DATA_DIR/audit/credential-resolutions.jsonl` (or the default data
root). It contains only credential/profile IDs, opaque backend version,
timestamp, and outcome. Locators, values, bootstrap authentication, and value
digests are never recorded. Failure to persist this audit record fails the
dependent launch. Session/campaign purge preserves this installation-wide audit;
`interceptor purge --all` removes it after confirming its writers are inactive.
Credential configuration and secret-store contents are preserved. See the accepted
[file-management requirements](file-management.md) for purge details.

`--model-api-key-env` remains a local-development
compatibility sources. `--production` rejects them. Bedrock, Vertex Gemini, and Azure OpenAI can instead
use direct workload identity; use `--provider-auth workload-identity` for
Vertex or Azure. No secret-store profile or resolution event is included in a
CapabilityManifest or EvidenceBundle.
