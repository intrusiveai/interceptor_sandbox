# Contract scope and maintenance

The embedded [contract](contract.json) describes only the Interceptor API used by
Operator's integration: public routes and native operations, their inputs,
outputs, prerequisites, errors, and behavioral guarantees. It excludes
administrative CLI commands, build/setup procedures, service/provider inventories,
and Interceptor-only configuration. It is not a target capability manifest.
Its version is independent of the product release version.

Operator pins the supported contract version and complete document SHA-256, not an Interceptor source
commit or the commit that introduced or published the contract. Interceptor can
release new commits with unchanged contract version and bytes without requiring an
Operator update. Source revisions and artifact hashes remain release provenance
and integrity records, not additional compatibility pins.

Change the contract and its version together when that agreement changes. First
identify the affected Operator API call, required input, consumed output, or
behavioral guarantee. Record that dependency in the pull request. An internal
implementation change or an Interceptor-only feature does not require a version
bump when the agreement remains unchanged.

For example, changing an API response Operator consumes or adding an operation
Operator must support affects the contract. An HTTPS seam configured and operated
entirely by Interceptor, without a required Operator interaction, does not.

Keep these responsibilities separate:

- The contract describes supported Operator-facing API behavior, including how to
  retrieve and interpret the separate environment capability manifest.
- The build-generated environment capability manifest describes availability in
  a particular sandbox for campaign planning and execution.
- Interceptor owns runtime configuration, credentials, policy, and enforcement.

A new feature does not automatically belong in either the contract or the
capability manifest. If its Operator dependency is unclear, clarify that boundary
with the user before changing the contract. See the repository
[agent instructions](../../AGENTS.md).

## Document identity

`POST /v1/contract-version` returns `interceptor_contract_version` and
`interceptor_contract_sha256`. The latter is 64 lowercase hexadecimal SHA-256
digits over the complete embedded UTF-8 file bytes, including formatting and the
final newline. It is not a release-archive, binary, capability-manifest, or API
response hash. The digest value is not stored inside the document it hashes.

`interceptor contract --json` emits those exact bytes for optional developer and
release verification. Reformatting the file changes its hash; a documentation-only
edit need not change the version when the API agreement remains unchanged.
Campaign compatibility requires exact equality of both the version and document
hash with the consumer's independently recorded pins. A document edit, including
formatting alone, blocks consumers retaining the old hash even if the version is
unchanged. Update pins deliberately after review; never learn the expected hash
from the running service. Users need not export a document.

Contract `0.2.0` adds the document digest to the identity API response. That API
addition justifies this version change; narrowing the previous feature inventory
alone would not. Release metadata continues to derive the version from this file.
