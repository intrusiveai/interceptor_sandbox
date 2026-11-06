# OpenAPI validation schema

`openapi-3.1-schema.json` is the unmodified OpenAPI Initiative validation schema
from https://spec.openapis.org/oas/3.1/schema/2022-10-07, downloaded for offline
contract validation. Its `$id` records that source. The OpenAPI Specification is
licensed under Apache-2.0: https://github.com/OAI/OpenAPI-Specification/blob/main/LICENSE.

This is test tooling, not part of the embedded API contract. Component schemas
are additionally compiled with the existing JSON Schema 2020-12 validator;
OpenAPI structural validation alone does not validate all Schema Object keywords.
