# Repository instructions

## Interceptor/Operator contract scope

The Interceptor/Operator contract describes only the Interceptor API capabilities
used by Operator's Interceptor integration: endpoints and operations, requests,
responses, prerequisites, errors, and behavioral guarantees. It is neither a
target application's capability manifest nor an inventory of Interceptor features.
Administrative CLI commands, build/setup procedures, provider/service inventories,
and Interceptor-only configuration MUST NOT be contract capabilities. The optional
contract export command is a developer/release tool, not an Operator API operation.

Before changing the contract, identify the specific Operator interaction affected:
an API call, required API input, consumed API output, or API behavior Operator depends on.
Describe that impact in the implementation proposal and pull request. An
Interceptor-only feature does not justify a contract change or version bump merely
because it adds functionality or configuration.

Keep environment-specific availability in the generated capability manifest and
runtime configuration and policy within Interceptor. Adding a feature does not
automatically require changing either the contract or the capability manifest.

- Contract-relevant: changing an API response Operator consumes or adding an
  operation Operator must support. Exported archive members are API outputs too:
  their field sets, schema versions, omission/encoding rules, hashes and provenance
  guarantees are contract-relevant when Operator parses or verifies them. An
  unchanged HTTP endpoint or binary media type does not make those changes internal.
- Interceptor-only: an HTTPS seam configured and operated entirely by Interceptor,
  with no required Operator interaction.

If the boundary is unclear, explain the proposed Operator dependency and clarify
it with the user before changing the contract. Do not infer an Operator dependency
solely from the presence of a feature in a blueprint or the Interceptor CLI.

See [contract scope and maintenance](internal/integrationcontract/README.md).

Operator compatibility pins the contract version and complete document SHA-256,
never Interceptor's source
commit or the commit that introduced or published the contract. Preserve source
revisions and artifact hashes for Interceptor release provenance and integrity,
without turning them into Operator compatibility or release requirements.

The contract MUST be a self-contained OpenAPI document with typed requests and
responses, not a prose capability inventory. Keep `info.version` authoritative,
all schema references local, and descriptions for behavior schemas cannot express.
Test actual API traffic and model/route drift when changing the definitions.
