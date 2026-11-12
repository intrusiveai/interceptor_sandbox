## Change

<!-- Describe the problem and resulting behavior. -->

## Validation

<!-- State what was verified and any remaining validation gaps. -->

## Operator integration

Does this change affect the API used by Operator's Interceptor integration? If so, how?

<!-- Identify the affected endpoint/operation, request, response, error, or guarantee
Operator relies on, and explain any contract/version change. Include structured
archive contents and native hash encoding consumed by Operator, even when the
HTTP route and binary response type stay unchanged. Otherwise state that
the integration agreement is unchanged. Interceptor-only features do not require
a contract change merely because they add functionality or configuration.
Administrative CLI commands, build/setup procedures and target-specific capability
inventories do not belong in this API contract. Keep contract version and digest
semantics distinct: document-only edits change the digest without necessarily
changing the API agreement, but still require a reviewed consumer hash-pin update
before campaign execution. Compatibility requires both version and document hash
to match; never pin an Interceptor source commit.
Describe any OpenAPI schema changes and validation against actual API exchanges.
See internal/integrationcontract/README.md. -->
