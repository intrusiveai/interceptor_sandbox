# Interceptor–Operator compatibility contract

Campaign compatibility MUST be established automatically through Interceptor's
API by comparing its contract version and complete-document SHA-256 with the
version and hash pinned by Operator's
Interceptor integration. Users MUST NOT need to export, inspect or supply a
contract to run a campaign. A written contract and its export command remain available for
integration development, inspection and release verification.

Implementation MUST be confined to this repository. Changes to Operator code,
configuration and GitHub Actions workflows are outside this specification's scope.

## Written contract

Interceptor MUST own and embed one self-contained OpenAPI 3.1 JSON document in
`internal/integrationcontract/contract.json`. It MUST describe only the API used
by Operator's integration. It MUST exclude administrative CLI commands, build/setup
procedures, general provider/service inventories and Interceptor-only configuration.
The capability manifest's returned structure MUST be defined, but target-specific
values and availability MUST remain in the separate generated manifest.

The document MUST define actual paths and methods, stable `operationId` values,
request and response schemas, required fields, types, permitted values, error
shapes, and binary evidence responses. Reusable models MUST be included under
`components.schemas`, with only internal references. Request/response bodies MUST
NOT be untyped objects where Interceptor defines a concrete structure. Fields
whose contents are intentionally application-defined MAY accept arbitrary JSON,
but MUST explain why. Do not encode runtime budgets as fixed schema limits.

`/v1/operations` MUST remain one HTTP route. Its request schema MUST use `oneOf`
variants that constrain `request.operation` and the corresponding body. Each
variant MUST link its specific success response with an `x-response-schema`
reference; this extension is necessary because OpenAPI associates responses with
HTTP operations rather than nested dispatch values. Standard OpenAPI response
schemas MUST also expose the union of those responses. Do not invent HTTP paths
for native operations. Lifecycle variants MUST similarly distinguish stop/restore.

Use descriptions for ordering, exact-byte digests, retries, ownership, uncertainty,
and evidence guarantees that schemas cannot establish. Existing wire types and
handlers MUST guide the schemas; tests MUST detect divergence. This replaces the
previous prose inventory rather than creating a second contract system.
Planned enhancements and settings for a particular environment MUST NOT appear as
supported product capabilities.

`interceptor contract --json` MUST export the embedded contract, including
`info.version` as the authoritative contract version, offline without Docker or
starting a target. The OpenAPI dialect version (`openapi`) MUST remain distinct.
The identity API, CLI version report and release metadata MUST continue using the
field name `interceptor_contract_version`, derived from `info.version`. The written
contract, API and CLI version reporting, and release metadata MUST use the same
authoritative contract version. The exported contract MUST describe the installed executable.

The contract version MUST be
independent of product release versions. Changes to
supported integration capabilities, interfaces or guarantees MUST update the
contract and its version together. Product changes that leave that agreement
unchanged, including documentation corrections, SHOULD retain the version.

Before changing the contract, the implementation MUST identify the affected
Operator API call, required input, consumed output, or relied-upon behavior.
Interceptor-only functionality MUST NOT trigger a contract change or version bump
merely because it adds a feature or configuration. An HTTPS seam configured and
operated entirely by Interceptor, with no required Operator interaction, is one
such example. Adding a feature MUST NOT automatically add it to the environment
capability manifest either. If the Operator dependency is unclear, it MUST be
clarified with the user before changing the contract. Follow the
[contract scope and maintenance guidance](../internal/integrationcontract/README.md).

## Automatic campaign compatibility check

Interceptor MUST add `POST /v1/contract-version` to its existing local HTTP API
in `internal/localapi`. The request MUST be an empty JSON object (`{}`), using
the existing strict JSON parsing and response framing. A successful response MUST
return `body.interceptor_contract_version` and `body.interceptor_contract_sha256`
as strings identifying the running service's embedded contract. The SHA-256 MUST
be 64 lowercase hexadecimal digits over the complete embedded UTF-8 document
bytes, including whitespace and its final newline. It MUST NOT hash a subset,
canonicalized JSON, API response, capability manifest, binary, or release archive.
The digest value MUST NOT be embedded in the document itself. The export command
MUST emit those same bytes unchanged so an independent hash matches the API.

This response extension MUST advance the API contract version to `0.2.0`.
Scope-only documentation corrections do not independently require a version bump.
This read-only endpoint MUST be available
before campaign attachment, require no campaign or session identifier, and perform
no attachment, target calls or state changes. Reuse the existing listener and
trusted-local-host access boundary; no separate service or listener is needed.

The request MUST NOT require the caller to already know the contract version.
The endpoint and its minimal response shape MUST remain stable across contract
updates so an incompatible consumer can still obtain and report the version.
Returning the digest MUST support exact hash matching without runtime document
retrieval or a user-managed compatibility step.

The documented consumer agreement MUST be simple: Operator's integration pins
one supported contract version and the SHA-256 of its reviewed complete document
bytes, and automatically calls this endpoint on the
Interceptor API instance it will use at the start of each campaign, before
attachment or campaign target operations. Exact equality of both values
permits the campaign to proceed to its normal environment and policy checks.
A mismatch MUST produce a clear error showing the expected and actual contract
versions and hashes and stop execution before any campaign target operations. A missing,
malformed or unreadable version or hash, unavailable endpoint or failed API request MUST
also stop execution, with an actionable compatibility error. There MUST be no
CLI fallback, silent fallback, version-range matching or negotiation among
historical contracts. Expected pins MUST be recorded with the integration after
review, never derived from the service being checked or refreshed automatically.
Any change to contract bytes, including whitespace-only edits with an unchanged
version, MUST fail compatibility until the consumer's hash pin is updated.

Campaign compatibility checking MUST NOT invoke a CLI command, read the full
contract, compare capability lists from it or consult an Internet catalog.
Matching product release versions
MUST NOT be required. Operator compatibility and release metadata MUST NOT require
pinning Interceptor's source commit or the commit that introduced or published the
contract. Different Interceptor commits and releases with the same contract version
and exact contract bytes MUST remain compatible without an Operator update. This consumer agreement defines the handoff for the separate
Operator implementation; this task MUST implement Interceptor's endpoint and
repository-local verification of that handoff. Existing `interceptor run` startup
behavior MUST remain unchanged: this check gates campaign attachment and operations,
not the initial creation of the sandbox hosting the target.

## Keep environment and policy separate

The environment capability manifest MUST remain the build-generated description
of what the particular sandbox supports, shared before campaign execution. Reuse
its existing format, generation and integrity rules. Matching contract versions
MUST NOT imply that every capability is enabled in every environment or replace
environment capability checks. The full contract MUST NOT become a second
environment manifest.

Runtime configuration, credentials, limits and enforcement MUST remain with
Interceptor. The contract and manifest do not grant permission. Do not hard-code
timeouts or size constraints unless necessary for reliable parsing or transport;
reuse existing wire requirements and justify any new fixed bound.

## Release packaging and publication

- Source revisions and artifact checksums MUST remain Interceptor release
  provenance and integrity records, not additional Operator compatibility pins.
  Recording them during Interceptor qualification MUST NOT require Operator to
  track each Interceptor release or republish its integration.
- Interceptor MUST provide `interceptor version --json` for inspection and release
  validation, exposing `interceptor_contract_version` alongside the product version
  and source revision. Both this command and `interceptor contract --json` MUST
  work offline without Docker or a running target. Plain-text version output MUST
  remain unchanged. These commands MUST NOT be the campaign compatibility check.
- `scripts/build_release.py` and `scripts/publish_release.py` MUST carry
  `interceptor_contract_version` through existing release metadata, verification
  and catalog-entry generation. Its value MUST match `interceptor contract --json`
  from the actual archived executable, not an unrelated installation or the
  product release version. Reuse the existing build manifest and integrity checks.
  Catalog entries MUST omit `contract_package_version`.
- The contract version MUST use the catalog's exact `major.minor.patch` format:
  no `v` prefix, ranges, suffixes, whitespace or leading zeroes, with a maximum of
  128 characters. This is an existing catalog parsing constraint.
- `.github/workflows/verify-release.yml` MUST exercise both the contract export
  and JSON version query from
  every packaged Linux and macOS CLI on a compatible runner and verify agreement
  across all published platforms and packaged Linux companions. It MUST retain
  verification evidence tied to archive checksums and source revision, and require
  those checks before publication. Existing reproducibility, archive integrity
  and runtime qualification checks MUST remain in place; checking a source file
  or only a macOS archive's Linux companion is insufficient.
- Interceptor publication MUST NOT depend on another repository's validation
  workflow or product-specific test suite. Existing Interceptor integration tests
  may use dependencies without qualifying those dependencies' releases. Shared
  publisher tests using synthetic catalogs MUST remain in place.
- `tools/release-publisher` MUST be upgraded to a complete reviewed export that
  supports `interceptor_contract_version`, including its `publisher-source.json`
  provenance. Review and test the replacement, including removed files; do not
  patch individual vendored files or use a working-tree export for publication.
  The updated publisher MUST be present at each new publishing tag. Existing
  immutable tags MUST NOT be moved to retrofit this support.
- `.github/workflows/publish-catalog.yml` MUST validate the generated entry before
  acquiring AWS credentials and publish through the committed local publisher.
  Preserve the protected release-tag execution, `production` environment and
  `CATALOG_PUBLISHER_ROLE_ARN`, conditional S3 writes, retry protections and exact
  CloudFront invalidation. No cross-repository workflow call, runtime fetch of
  private publisher code, service deployment or IAM change is required.
- Candidate publication MUST include the verified contract version with
  `tags: ["rc"]` and the actual uploaded artifact identities. Promotion to
  `tags: ["release"]` MUST preserve that version and the exact qualified bytes.
  A rebuilt candidate MAY change the contract version only together with changed
  artifact identities and repeated qualification. Stable entries MUST reject
  replacement or demotion, including stale candidate retries.

## Verification and documentation

Tests MUST verify that the full contract export and JSON version query agree,
that plain-text version output remains unchanged, and that both commands work
offline without Docker or starting a target. API tests MUST verify the endpoint's
response framing, strict request parsing, availability before attachment, absence
of state changes or target calls, and agreement with the embedded contract and CLI
reporting.

Tests MUST validate the OpenAPI document against the official structural schema
and compile all component schemas with the existing JSON Schema validator,
offline. Tests MUST reject broken/external references and verify request and
response validation rejects missing fields, wrong types, unknown operations and
bodies belonging to a different native operation. Tests MUST compare serialized
Go model fields with schema fields and validate representative actual API traffic.
The packaged consumer qualification MUST validate its JSON requests/responses,
including operation-specific responses, against the embedded OpenAPI definitions.
Schema validation supports development and release testing; campaign startup
MUST continue to use only version/hash identity, not full-document validation.

Tests MUST compare the documented routes/native operations to the implemented
public API inventory and reject non-API operations such as administrative CLI
commands. They MUST exercise the real identity endpoint and independent consumer
check, verify the digest against exact exported bytes, and check malformed or
missing identity fields. Packaged runtime qualification MUST compare the running
API's document hash with the same installed executable's complete contract export
before campaign attachment. Evidence/archive hashes MUST remain distinct.

A repository-local consumer fixture MUST independently pin a supported version
and complete-document hash, call the HTTP
endpoint served by an actual release executable, and demonstrate equality followed
by attachment and a normal operation against a built application. It MUST
demonstrate rejection before attachment or campaign target operations for
mismatched, missing, malformed and unreadable versions or hashes, an unavailable endpoint,
and failed API requests. The compatibility check MUST invoke neither CLI command
nor a full-contract export. Include different product versions with the same
contract version and bytes and confirm environment capability checks still apply.
Tests MUST reject altered contract bytes (including formatting-only edits) with
an unchanged version and show that hash mismatch prevents attachment. This MUST
NOT require Operator changes or be reported as full Operator integration validation.

Release tests MUST reject missing or invalid contract versions, platform
disagreement and metadata that does not match the actual artifact. Run the updated
publisher's tests and exercise candidate creation, replacement and promotion with
local fixtures, including rejection of changed contract metadata on promotion.
Publication triggers and approval gates MUST remain intact; verification alone
MUST NOT publish. Authorized release validation MUST check the public catalog's
contract version and, after promotion, the site's **Interceptor / Operator
contract** indicator without redeployment. Candidates MUST remain hidden on the
site. Report local, CI and live-publication results separately, including gaps.

Documentation MUST explain the API endpoint, automatic exact-version-and-hash check, mismatch error,
optional full-contract export and separate environment manifest in the existing
integration guide. Release-maintainer guidance MUST describe when to update the
contract version, the metadata checks and the publisher upgrade in
`.github/maintainers/CANDIDATE_RELEASES.md`. Campaign instructions MUST NOT add a
manual contract-export or comparison step. No new service or configuration system
is needed.
