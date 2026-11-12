# Contract scope and maintenance

The embedded [OpenAPI 3.1 contract](contract.json) describes only the Interceptor API used by
Operator's integration: public routes and native operations, their inputs,
outputs, prerequisites, errors, and behavioral guarantees. It excludes
administrative CLI commands, build/setup procedures, service/provider inventories,
and Interceptor-only configuration. It is not a target capability manifest.
Its `info.version` is independent of the product release version and the OpenAPI
dialect version (`openapi`). API and release reporting retain the field name
`interceptor_contract_version`.

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

Contract `0.2.0` introduced the document digest to the identity API response. That API
addition justifies this version change; narrowing the previous feature inventory
alone would not. Release metadata continues to derive the version from this file.

## Maintaining the OpenAPI definitions

Edit the checked-in OpenAPI JSON document directly. Define paths, methods, stable
operation IDs, typed request/response bodies, required fields, permitted values,
and reusable models. Keep every reference inside the file so the digest covers
all definitions. Preserve exact-byte exports; never hash only the schema subset.

The native endpoint uses `oneOf` request variants selected by `request.operation`.
Each variant's `x-response-schema` reference identifies its specific successful
response; the HTTP operation also exposes the full response union for ordinary
OpenAPI tooling. This is a dispatch mapping, not an additional compatibility
manifest. Do not invent routes for native operations. Arbitrary JSON is reserved
for explicitly documented application-defined payloads, schemas and observations.

Run the contract, local API and consumer tests. They check OpenAPI structure,
compile schemas offline, compare route and Go field inventories, and validate
actual API exchanges. The packaged smoke client also checks operation-specific
responses. Retain behavioral tests for retry, ordering, evidence and restore
semantics: schema validation cannot prove those guarantees.

Review the final document and update the consumer fixture's version/hash pins
explicitly. Do not generate the expected hash during tests or learn it from the
running service. Operator maintainers can use this specification to validate their
integration during development; startup still requires only the identity API.

A native operation record can contain the exact unassigned response
`{"status":0,"session_revision":0}` only while `state` is `running`. Keep this
exception scoped to the record; do not add it to general HTTP response schemas
or lifecycle records. Test status polling during an actual blocked invocation
and after completion. Correcting the schema to describe this existing behavior
was a correction within `0.2.0`; subsequent agreement changes advance the version.

## Evidence archive compatibility

Contract `0.3.0` includes the structured contents returned by `POST /v1/evidence`.
Operator verifies these records, so their fields, supported schema versions,
serialization and digest rules are part of the API agreement. HTTP 200 and an
unchanged tar media type do not establish format compatibility.

The response's `x-evidence-members` maps archive paths to typed local schemas and
records JSON, JSONL or raw-byte encoding plus required/conditional membership.
This extension describes the binary payload; it is not a second manifest or a
runtime negotiation mechanism. `x-evidence-integrity` specifies chain, binding,
blob and completeness checks which JSON Schema alone cannot establish.

File manifests support schema 2 with required logical `application`/`supervisor`
ownership, and historical schema 1 without ownership. Journal transactions still
use schema 1; filesystem operation ownership is distinct from that version.
Historical records may omit owner; never guess ownership or infer that historical
evidence can be restored. New ownership-aware file operations include owner.

Native hashes use Go `encoding/json.Marshal`, not JCS or the pretty-printed member
bytes. Model annotations retain field order and `omitempty` behavior. Ownership
participates in the digest. Preserve integer precision, raw JSON ordering and
escaping; do not ignore unknown fields to bypass verification failures.

Before changing an exported record, update its schema, relevant encoding rules,
contract version and reviewed qualification pins together. Run field/order drift
checks, negative ownership/hash tests, and actual exporter tests. The packaged
smoke client validates downloaded archive members and native record hashes.
Retained QA exports can be checked with the opt-in `TestRetainedEvidenceArchives`
test using newline-separated `INTERCEPTOR_CONTRACT_EVIDENCE_ARCHIVES` paths; keep
customer archives outside the repository. These producer-side checks complement
Operator's independent provenance verifier; validating that consumer remains a
separate integration qualification requirement. Updating its pins alone cannot
repair a parser that does not support the declared formats.

## Injection target discovery

Contract `0.4.0` adds endpoint metadata consumed through `capabilities.read`:
`http-route-set` uses exact method/path allowlists (empty means unrestricted,
except HEAD); `state-collection` uses issuer-scoped record keys and mutable field
names. These are selectable targets for the existing `injection.arm` operation,
not new API routes. Keep generated manifests, runtime validation, OpenAPI types
and native discovery-to-injection tests aligned. Operator must separately handle
these forms before adopting the new version/hash pins.
