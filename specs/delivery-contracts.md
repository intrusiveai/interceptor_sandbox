# Blueprint delivery contracts and benign-data injection

This specification extends MVP Sections 6 and 10 and adaptive requirement
INT-AEF-AD-003. Interceptor owns the native delivery descriptions; consumers such
as Operator project them rather than defining a competing native format.

## INT-DEL-001 — version and source binding

Blueprints using application `delivery` / `delivery_asset`, or service
`export_contracts` / `implementation`, MUST select
`api_version: interceptor.dev/v1alpha2`. Existing v1alpha1 Blueprints remain
supported and MUST produce capability-manifest/v1alpha3 without delivery metadata.
Both Blueprint versions MUST use the same typed-JSON native digest recipe.
A v1alpha2 Blueprint produces `interceptor.dev/capability-manifest/v1alpha3`
with `delivery_schema_profile: interceptor.delivery-schema/v1`. Other adaptive
resources keep their existing versions. Unknown Blueprint fields/versions fail.

Each application operation MAY supply one inline `delivery` object OR one
`delivery_asset` path, never both. The latter names a complete JSON contract
file inside the Blueprint directory, not a schema URL. It MUST be a regular,
bounded, unlinked file with no symlink path components or path traversal.
Strict decoding rejects unknown contract fields, duplicate JSON keys, invalid
UTF-8/surrogate escapes, trailing values and excessive nesting. Referenced
contract bytes enter the ordinary sealed asset inventory. Inline contracts
enter the Blueprint hash. Both are bound to the sealed application digest.

Validation resolves contracts from these sealed sources. Offline capabilities,
host session binding and the in-container capability endpoint use that same
resolved representation and builder. Derived fields are not added to the
Blueprint hash preimage. The manifest includes the resolved contract, not its
asset path. No session, execution owner, current budget or discovery timestamp is needed.
A consumer MUST verify the native version/digest and bind its own projection
version. Native hashing remains sorted native Go-struct JSON, with `digest`
present as the empty string before SHA-256; it is not Operator JCS.

Missing application delivery information is a specific preparation gap:
v1alpha2 emits a validation warning and operation
`delivery_status: missing-input-contract`, without guessing a contract.
A described operation emits `delivery_status: described`; this is not a
claim of live readiness, successful delivery or authority to invoke.
Consumers cannot expose an operation requiring missing input information as
ready. They may still use sufficiently described operations/surfaces.

## INT-DEL-002 — contract structure and validation

The shared Go owner is `internal/delivery`. A contract has:

| Field | Requirement |
| --- | --- |
| `description` | Required nonblank public description, at most 4,096 bytes. |
| `input` | Required body description for decoded application input, HTTP route body or MCP tool arguments, according to its enclosing endpoint. |
| `output` | Required body description; unknown structure is explicitly allowed. |
| `examples` | Optional list of at most eight benign input/output pairs. Each has a unique `id` (1–128 bytes), `input` and `output`. |

Each body has required `media_type`, `encoding`, `max_bytes` and `basis`,
and optional `schema`:

- Encoding is `json`, `utf8` or opaque `bytes`. JSON requires
  `application/json` or a `+json` media type. Text schemas validate a JSON
  string representing the text; bytes cannot assert a JSON schema.
- `max_bytes` is 1–2,097,152 decoded bytes. Native transport/runtime limits may
  be lower and always win. There are no inferred defaults for required fields.
- Basis is `provider-declared`, `source-derived`, `observed`,
  `synthetic` or `unknown`. This is reviewed provenance, not runtime
  attestation. Input cannot be unknown; JSON input requires an explicit schema
  (including an explicit unrestricted object `{}` when that is truly the
  interface contract). Unknown output omits `schema`.
- Field descriptions and required/optional fields belong in the local schema.
  Examples validate against their corresponding body descriptions and limits.
  Each encoded example input/output is at most 16 KiB. JSON examples are JSON
  values; text/byte examples are literal UTF-8 strings, not base64.
- A contract is at most 256 KiB encoded JSON, each schema at most 64 KiB, and the
  combined resolved application/service contract inventory at most 1 MiB.
  A service exports at most 128 endpoints.

The implementation supports a deliberately closed JSON Schema 2020-12 profile:
`$schema` (only that dialect), `type`, `title`, `description`,
`enum`, `const`, `required`, `properties`,
`additionalProperties`, `items`, string length, numeric bounds
(including exclusive bounds), array length and object property-count bounds.
Schema nesting is at most eight levels / 256 schema nodes, with at most 128
properties per object. Nested boolean schemas are supported; root schemas are
objects. Unsupported keywords, including references, regexes, formats,
combinators and default insertion, are rejected, not silently ignored.
No schema resolver may read files, make network requests or run code.
Importers must provide an explicitly reviewed representation in this profile;
they must not strip unsupported constraints while claiming an exact import.

Operation `maximum_input_bytes` is the minimum of its declared ceiling,
native 2 MiB turn-input ceiling and configured blob ceiling. Input validation
checks encoding, media type and schema before application invocation on both
the turn API and direct application bridge. It does not coerce or repair input.
Command JSON stdin / JSON-pointer argv bindings require JSON input; code and
external protocol contracts describe the actual JSON input value explicitly.
Schemas constrain wire packaging, not attack ideas. Benign examples are not
payload allowlists.

The turn API records optional `output_contract_status` on its response and
attack-channel `turn_completed` event: `matched`, `unknown` or `mismatch`.
No-contract legacy turns omit it. This check is advisory: a schema mismatch
alone does not change application status, discard bounded output or repair it.
The direct reverse proxy and managed-service responses do not claim equivalent
output-schema conformance reporting. Existing hard output/resource limits,
error handling, redaction and evidence access rules still apply.

## INT-DEL-003 — service surfaces and uncertain MCP output

A v1alpha2 service may set `export_contracts: true` to explicitly publish
reviewed endpoint context. Supported kinds are `fixture_http`,
`stateful_http` and `mcp_http`. False/omitted exports no endpoint inventory.
This opt-in is a disclosure decision: authors must review descriptions,
schemas and examples for sensitive content.

Endpoint entries have stable `id`, `kind` (`http-route` / `mcp-tool`),
optional logical `method` / `path` / `collection`, optional `delivery`,
`injection_root`, optional `output_pointer`, and
`maximum_request_bytes`. The latter is the native 1 MiB service request-body
limit, including the entire JSON-RPC envelope for MCP. Consumers account for
envelope overhead as well as decoded argument limits.

- HTTP fixture rules and stateful routes accept optional inline `delivery`.
  Export reuses their stable IDs, methods, logical paths and collection names.
  An absent contract remains absent; it is not inferred from response fixtures
  or records. These descriptions do not replace the managed handlers' existing
  matching/state-mutation validation.
- MCP export reuses `input_schema`, optional `output_schema` and tool
  description from the existing tools asset, with no second authored schema.
  Optional tool `contract_basis` defaults to `synthetic`; optional
  `delivery_examples` supplies explicitly public benign pairs.
  Export validates these against the closed schema profile.
- MCP input means the arguments object, not the complete JSON-RPC request.
  Its output schema describes `structuredContent`, not arbitrary text or the
  outer protocol envelope. Without an output schema the exported output basis
  is `unknown`, even when input provenance is known. Both MCP decoded
  descriptions have a 1 MiB ceiling; the existing encoded-result ceiling also
  applies, including duplicated text/structured representations.
- The injection root for native MCP is the successful `tools/call` result
  object (`mcp-tool-result`); `output_pointer` is `/structuredContent`.
  Placement pointers are relative to that result. HTTP uses `response-body`.
  Existing injection profiles describe supported selectors, placements and
  carriers. A schema/example does not prove a pointer exists on a future call.
- With unknown output, a caller can select the protocol-level
  `/content/0/text` if present and string-valued, or use an explicitly
  reviewed synthetic record contract. Missing pointers/wrong types produce
  recorded placement failures and leave the response unchanged; no guessed
  field or fallback pointer is allowed. An MCP tool error is not a successful
  response-injection candidate.

The Blueprint author may obtain provider tool metadata, public documentation,
reviewed source or authorized sanitized samples before sealing. Samples are
observations, not exhaustive schemas. The author need not predict every remote
response; the local simulation needs only explicit benign behavior for the
workflows being assessed. Absent simulation behavior remains a coverage gap,
not a reason to forward to production or fabricate successful results.
No Interceptor runtime or attack-harness schema discovery contacts the remote
third-party MCP service.

Export never copies fixture match values/results, seed records, listener
origins, credentials, auth configuration or canaries automatically. Safe
logical paths/tool names/JSON Pointers are selection context, not host routes.
Filesystem capabilities retain logical namespace IDs and limits; source asset
paths and private container/host roots are not published. Any supplemental
namespace-relative filename examples must be separately reviewed context.

## INT-DEL-004 — first-class MCP implementation extension

`mcp_http` has a release-owned `implementation` identifier. Empty and
`generic` select the existing fixture/stateful implementation. Unregistered
names fail Blueprint validation; an identifier is not an executable, module
path, remote server URL or authority to download code.

Adding a registered implementation requires a typed validated configuration,
local service factory, deterministic benign seed/fixture behavior, capability
export and conformance tests together. It must use the common MCP transport,
authorization, injection, recording and snapshot/state interfaces. It must
preserve caller-supplied benign data and source immutability. It cannot grant
external network access or bypass the shared injector. No Jira, Zendesk or
other named third-party implementation is shipped by this change.

## INT-DEL-005 — seed data and injection lifetime

Blueprint service seeds/fixture assets and filesystem seed assets are immutable
sources. Writable filesystem seeds are copied, not writable bind-mounted or
hard-linked from those sources, into a specific session's mutable roots.
A fresh session materialized from the same sealed source starts with benign
records unless an explicit snapshot restore replays prior mutations.

`mcp_tool_result` and `service_response` mutate a served copy after
authorization and real service handling. They do not write the injected content
back to the benign seed or managed record. Legitimate state-changing tool calls
still have their ordinary state effects. MCP text and structured content are
distinct representations: selecting one does not silently rewrite the other.

Existing `environment_state` fidelity mutation is retained for managed
records and files. File mutation changes only the sandbox session copy and
persists for later reads until changed or restored. It is not per-read
interception. CSV is a text file, not a built-in row/cell service: callers use
the existing supported whole-text/root replacement or text placement rules.
There is no implicit CSV parser, cell selector or automatic record merging.
JSON files retain existing JSON-pointer placement behavior. Read-only source
assets cannot be fidelity mutation targets. Deleting/disabling an injection
does not undo a previously applied fidelity write.

Tests must demonstrate unchanged source bytes, persistent session-copy
mutation, pristine independently materialized sessions, transient MCP served
record changes, unchanged unselected fields, and failed missing-pointer
placement without fabricated data.

## Verification

`internal/delivery/*_test.go` covers schema/encoding/error boundaries and
decoder fuzz seeds. Environment tests cover asset sealing, strict decoding,
legacy gates and missing-contract warnings. Adaptive tests cover versioned
digests, limits and detached public copies. Service and sandboxd tests cover
served-copy and file-source isolation. These unit/in-process tests do not
replace live container, gateway, ownership, Operator or model integration gates.

## Managed database extension

The [PostgreSQL integration contract](postgresql-integration.md) extends managed
services with `postgresql`, independent `postgresql+tls` transport, the
`database_response` simulation surface, policy-constrained `environment_state`
database mutations, and `database_query_match` oracles. Database response
selection MUST use query/parameter predicates and result/cell ordinals; HTTP
selectors and HTTP endpoint contracts MUST NOT describe this transport.
Capabilities MUST expose only public table policy and supported surfaces, never
credentials or oracle SQL. Native backups MUST use integrity-covered
`database_checkpoint` journal operations; observation windows and required
capture-failure policy MUST apply to native database evidence.
