# Discoverable attacker-response and identity-state injection targets

## Required behavior

- A capability manifest MUST describe selectable targets for dynamic attacker
  response injection and identity state injection using existing endpoint,
  delivery-body, selector, placement and error mechanisms. It MUST NOT introduce
  another injection API, service, or blueprint authoring requirement.
- Discovery MUST work with both supported blueprint versions. Existing HTTP/MCP
  contract-export opt-ins MUST retain their behavior.
- Dynamic attacker services MUST export an endpoint of kind `http-route-set`
  with `injection_root: response-body`. `methods` and `paths` MUST describe
  independent, sorted, deduplicated exact allowlists. Omitted/empty lists MUST
  mean unrestricted values, not guessed routes, templates or wildcard strings.
  A consumer MUST select a concrete method and absolute URL path for injection.
- HEAD MUST NOT be a response-injection target because its body is not delivered.
  A HEAD-only sink MUST NOT advertise response injection. Static sinks MUST NOT
  export injectable endpoints. Request capture MUST remain supported.
- Export MUST describe body encoding and effective request limits without
  exposing configured response values or request captures. It MUST NOT infer
  a detailed response schema from fixture data.
- Validation, discovery and startup MUST share allowlist parsing. Malformed
  lists MUST fail rather than broaden access. Arming an out-of-allowlist target
  MUST use the existing `invalid_injection` error; runtime checks MUST remain.
- Identity services MUST export `state-collection` endpoints using the existing
  `collection` field and `injection_root: service-record`. Each endpoint MUST
  list its existing `keys` and `mutable_fields`, with delivery input/output
  schemas describing the complete record, not the injection envelope.
- Grants and delegations MUST be scoped to their owning issuer, omit empty
  collections and describe mutable `active` and `scopes`. Faults MUST describe
  the existing `issuer` key with `issue_expired`, `reject_exchange` and
  `unavailable`. Descriptions MUST NOT disclose credentials, principal claims,
  scope values or current record contents.
- These descriptions MUST NOT permit creation, deletion, cross-issuer access,
  or mutation of immutable identifying fields. Existing managed-state validation,
  journaling, evidence failure handling and snapshots MUST remain authoritative.
- Export MUST be deterministic, bind all new metadata into the existing manifest
  digest, and detach mutable blueprint storage.

## Contract and documentation

- The OpenAPI contract MUST describe the added endpoint kinds and fields as
  consumed outputs of `capabilities.read`, including wildcard semantics and
  identity selector construction. The integration contract MUST advance to
  `0.4.0`, with reviewed independent qualification version/hash pins updated.
- Documentation and maintenance guidance MUST explain how consumers turn the
  descriptions into existing injection selectors, and that Operator needs a
  separate consumer update. No Operator repository changes are part of this work.
- Existing wrappers MUST be rebuilt to bind the new capability manifest.

## Verification

- Tests MUST discover targets through the native API, register payloads/attempts,
  arm injections using advertised selectors and demonstrate changed attacker
  responses and identity authorization behavior over both existing transports.
- Coverage MUST include restricted/unrestricted and partly restricted sinks,
  static/HEAD-only sinks, JSON/text responses, malformed allowlists, forbidden
  routes, multiple issuers, immutable fields, unknown collections and keys.
- Actual capability responses MUST validate against OpenAPI. Tests MUST check
  deterministic digests, secret-free export, detached storage and record schemas.
