# Host execution and authentication alignment

Operator and Interceptor MUST default to foreground execution. The owning process
MUST inherit the caller's host environment, accept cancellation, and complete
bounded shutdown cleanup. Neither component MUST resume an interrupted campaign
or session. Journals, exact Docker identity, administrative termination and purge
remain available independently of the owning process.

Operator `campaign start` and `run` MAY use explicit `--service` for independent
execution; Interceptor MAY run its same foreground command in the optional service
templates. A service MUST use its configured account/environment, MUST NOT copy
terminal secrets into service definitions, and MUST disable automatic restart.
For GitHub Actions, run both foreground commands within the authenticated job;
keep Interceptor running while Operator executes, then explicitly stop and wait
for it in job cleanup. A detached service MUST NOT be assumed to inherit job auth.

## Common selectors

Both components MUST support these host-only authentication selections. Operator
uses `authentication` in its model profile; Interceptor uses `--provider-auth`.
Secret-store profiles use `authentication` in both, independently of model auth.

| Selector | Model routes | Secret store | Bootstrap |
| --- | --- | --- | --- |
| `workload-identity` | Bedrock, Azure OpenAI, Vertex | AWS, Azure, Google | AWS web-identity token file/container or instance role; Azure federated token file or managed identity; Google external-account ADC or attached identity |
| `aws-profile` | Bedrock | AWS | Explicit `aws_profile` / `--aws-profile`; SDK SSO, assume-role, credential process or shared-file credentials |
| `aws-environment` | Bedrock | AWS | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, optional `AWS_SESSION_TOKEN` |
| `azure-cli` | Azure OpenAI | Azure | Existing `az login` cache, including `azure/login` OIDC |
| `azure-client-secret` | Azure OpenAI | Azure | `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET` |
| `google-adc` | Vertex | Google | ADC including local user, service account, impersonation, federation and attached identities |
| `api-key` | All HTTP model providers | Not a vault login | Model key resolved using `credential_id` / `--model-credential-id` |
| `api-key-env` | All HTTP model providers | Not a vault login | Explicit `api_key_env` / `--model-api-key-env` naming a host environment variable |
| `workload-token` | Anthropic | Not a vault login | Secret-store bearer token; administrator owns issuance/renewal |
| `none` | LiteLLM | None | Explicit unauthenticated, fixed administrator-selected gateway |

`secret-store` MUST remain an accepted alias for model `api-key` authentication.
Secret stores SHOULD be the default for model keys. Environment keys MUST be
supported only through explicit host selection (Interceptor retains its existing
nonproduction CLI compatibility path). They MUST NOT be mounted into either guest,
embedded in requests from guests, logged, journaled, reported or written into start
records/service files. Values MUST NOT appear in command-line arguments.
Missing selected credentials MUST fail without falling back to a different mode.
Mode selectors and variable names are private configuration, not credentials.

For AWS stores, `aws_profile` without `authentication` MUST continue to select that
named profile; other omitted cloud-store selectors mean workload identity.
An explicit profile MUST NOT be combined with environment mode. Shared AWS files
MUST be excluded unless a named profile is selected. Environment mode MUST select
only the environment pair/session and ignore ambient `AWS_PROFILE`/shared files.
Short-lived sessions SHOULD be preferred over long-lived keys.

The two components MUST accept either `operator.dev` or `interceptor.dev`
`secret-store-profile/v1alpha1` and `host-credential-ref/v1alpha1` namespaces in
credential configuration. The same `profiles`/`credentials` file, field names,
locators and allowlists can therefore be shared. This does not grant access to
secrets: the selected bootstrap identity still needs the store's read permission.
Vault MUST support a private rotating token sink or fixed authenticated proxy;
the sink MUST be reread at uncached resolution. No component implements Vault login.

## GitHub Actions OIDC

Workflow jobs MUST grant `id-token: write`; source checkout normally also needs
`contents: read`. Administrators MUST configure cloud federation trust scoped to
the intended repository and branch/environment, and configure the action before
starting either process. Match the actual GitHub subject claim emitted for the
repository/environment; do not assume one universal subject format.

- AWS: use [configure-aws-credentials](https://github.com/aws-actions/configure-aws-credentials)
  with `role-to-assume` and `aws-region`, then select `aws-environment` for Bedrock
  and AWS secret profiles. The action exports temporary access keys and a session
  token. Give the role `bedrock:InvokeModel` on the selected models/inference
  profiles and `secretsmanager:GetSecretValue` on selected secrets; customer KMS
  keys also need `kms:Decrypt` and an allowing key policy. Trust must permit
  `sts:AssumeRoleWithWebIdentity` with the configured audience and subject.
- Azure: use [azure/login](https://github.com/Azure/login) with a federated app or
  user-assigned identity, client/tenant/subscription identifiers, then select
  `azure-cli` for both Azure OpenAI and Key Vault. Grant **Cognitive Services OpenAI
  User** on the model resource and **Key Vault Secrets User** on selected secrets
  (or equivalent legacy secret-get access). The CLI cache is the credential source;
  `workload-identity` instead requires an actual `AZURE_FEDERATED_TOKEN_FILE` with
  `AZURE_CLIENT_ID` and `AZURE_TENANT_ID`, not just a completed CLI login.
- Google: use [google-github-actions/auth](https://github.com/google-github-actions/auth)
  with `workload_identity_provider`, optional impersonated `service_account`,
  `create_credentials_file: true`, and `export_environment_variables: true`.
  Select `workload-identity` or `google-adc` for Vertex and Secret Manager. Grant
  **Vertex AI User** (or predict-only custom permission) and **Secret Manager
  Secret Accessor** on selected secrets. For service-account impersonation, grant
  **Workload Identity User** to the scoped federated principal on that account;
  quota-project use may also require **Service Usage Consumer**. Preserve the
  generated external-account file and its job environment until both processes exit.

Authentication does not grant model availability or bypass project API enablement.
The host image MUST contain the relevant SDK dependencies; Azure CLI mode also
requires `az` on the process PATH. Google ADC does not require `gcloud` at runtime;
local setup uses `gcloud auth application-default login`, which is separate from
`gcloud auth login`. Azure local setup uses `az login`; AWS SSO uses
`aws sso login --profile NAME`.

Static AWS action-exported session values do not refresh themselves. Configure the
role session duration to cover preparation, execution and cleanup. Other refresh
paths also depend on the source identity remaining available; no component promises
renewal after job end, logout or action post-cleanup. Authentication failure MUST
use existing terminal/uncertain-outcome handling, never replay or resume execution.
Synthetic tests establish credential-selection behavior; live cloud/OIDC trust,
permissions and token-lifetime qualification require the actual runner accounts.
