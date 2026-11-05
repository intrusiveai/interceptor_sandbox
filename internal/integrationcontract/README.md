# Contract scope and maintenance

The embedded [contract](contract.json) describes the agreement Operator relies on
when integrating with Interceptor. It is not a catalog of all Interceptor
functionality. Its version is independent of the product release version.

Change the contract and its version together when that agreement changes. First
identify the affected Operator API call, required input, consumed output, or
behavioral guarantee. Record that dependency in the pull request. An internal
implementation change or an Interceptor-only feature does not require a version
bump when the agreement remains unchanged.

For example, changing an API response Operator consumes or adding an operation
Operator must support affects the contract. An HTTPS seam configured and operated
entirely by Interceptor, without a required Operator interaction, does not.

Keep these responsibilities separate:

- The contract describes supported Operator integration behavior.
- The build-generated environment capability manifest describes availability in
  a particular sandbox for campaign planning and execution.
- Interceptor owns runtime configuration, credentials, policy, and enforcement.

A new feature does not automatically belong in either the contract or the
capability manifest. If its Operator dependency is unclear, clarify that boundary
with the user before changing the contract. See the repository
[agent instructions](../../AGENTS.md).
