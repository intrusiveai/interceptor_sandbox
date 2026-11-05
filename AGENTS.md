# Repository instructions

## Interceptor/Operator contract scope

The Interceptor/Operator contract describes behavior Operator must understand or
rely on to integrate with Interceptor. It is not an inventory of every Interceptor
feature.

Before changing the contract, identify the specific Operator interaction affected:
an API call, required input, consumed output, or behavior Operator depends on.
Describe that impact in the implementation proposal and pull request. An
Interceptor-only feature does not justify a contract change or version bump merely
because it adds functionality or configuration.

Keep environment-specific availability in the generated capability manifest and
runtime configuration and policy within Interceptor. Adding a feature does not
automatically require changing either the contract or the capability manifest.

- Contract-relevant: changing an API response Operator consumes or adding an
  operation Operator must support.
- Interceptor-only: an HTTPS seam configured and operated entirely by Interceptor,
  with no required Operator interaction.

If the boundary is unclear, explain the proposed Operator dependency and clarify
it with the user before changing the contract. Do not infer an Operator dependency
solely from the presence of a feature in a blueprint or the Interceptor CLI.

See [contract scope and maintenance](internal/integrationcontract/README.md).
