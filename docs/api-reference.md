# Native supervisor API reference

The public integration starts with `interceptor run` at `127.0.0.1:8080`.
See [Local Operator integration](local-api.md) for the supported public routes.
The endpoints below describe private supervisor operations used by the typed
v1alpha2 dispatcher and administrator CLI. `run` does not expose separate
application/control convenience URLs, and these private paths are not public
aliases. Public binding uses attach; restore and confirmed stop use the lifecycle
API. Native model-provider credentials remain host-only.

All control request bodiesAll control request bodies use strict JSON: misspelled or unknown fields are
rejected instead of ignored. Control responses are JSON and tell clients not to
cache them.
Errors use:

```json
{
  "error": {
    "code": "invalid_turn",
    "message": "description",
    "request_id": "req-1"
  }
}
```

## Application interface

For an HTTP application, the application base URL accepts the application's
native request and response format. Only method/path pairs declared by
`application.interface.operations` are forwarded; undeclared routes return
`501`.

```sh
curl -sS -X POST "$APPLICATION_URL/invoke" \
  -H 'Content-Type: application/json' \
  --data '{"prompt":"hello"}'
```

When `identity.inbound` is configured for an HTTP application, the harness can
add `X-Interceptor-Caller: <user-principal>`. Interceptor removes that
harness-only header and sends the application a freshly signed synthetic bearer
token instead. If inbound identity is configured and no caller is named, an
arbitrary caller-supplied `Authorization` header is removed rather than copied
into the sandbox.

For every other interface kind, Interceptor supplies a small, consistent HTTP
surface. Post the application's native input to the declared operation:

```sh
curl -sS -X POST "$APPLICATION_URL/operations/ask" \
  -H 'Content-Type: application/json' \
  --data '{"prompt":"hello"}'
```

The adapter converts that request to a command, stream exchange, function call,
or external-adapter frame. Use the cooperative control endpoint when you want
the structured turn envelope and stable turn ID. See [Application
interfaces](application-interfaces.md). Direct application-URL calls and
cooperative turns both consume the session's turn limit and are included in
activity/evidence tracking.

## Cooperative control endpoints

### `GET /v1/capabilities`

Returns the deterministic, secret-free CapabilityManifest bound to the sealed
Environment and Application. A ScenarioBundle or ExecutionLease must bind its
exact `digest`. Service entries report role, supported injection surfaces,
static/dynamic attacker response mode, available HTTP/HTTPS transports, and
bounded creatable collections.
`file_namespaces` reports logical IDs and ceilings without revealing container
paths. `cross_vm_operations` is the exact gateway operation vocabulary and
`injection_profiles` declares each available surface's scopes, placements,
selector fields, and response/state carrier classes. `network` reports the
closed DNS policy and query ceiling plus HTTP/HTTPS front-door and TLS trust
capabilities without exposing listener addresses or certificate material.

### `POST /v1/artifacts` and `GET /v1/artifacts/{digest}`

Registers and verifies concrete payload bytes:

```json
{
  "descriptor": {
    "digest": "sha256:...",
    "size_bytes": 97,
    "media_type": "application/json",
    "canonicalization": "raw"
  },
  "content": "BASE64_BYTES"
}
```

`GET /v1/artifacts` lists registered payload descriptors without bodies. Exact
registered content and target output referenced by an ObservationView can be
read by digest. Content reads are trusted operations; the operations gateway
requires `artifact.read` explicitly.

### `POST /v1/attempts` and `GET /v1/attempts`

Registers and lists immutable producer-neutral AttemptContexts. An artifact
must be verified first. Parents must already exist in the same campaign/thread,
and `attempt_index` must increase. See [Adaptive simulation
interface](adaptive-simulation.md).

### `GET /v1/observations/{attempt-id}`

Returns a fixed v1alpha2 ObservationView under the immutable feedback profile.
Select `?turn_id=<invocation-id>` or `?receipt_id=<fixed-receipt>`; omission is
allowed only for an unambiguous single invocation. The view includes category
availability, entry descriptors, recording cutoff and capture limits. Use native
`observation.content.read` for bounded bytes. See [live feedback](feedback.md).

### `GET /v1/status`

Returns session metadata, including identity, phase, environment/application
digests, resource limits and consumption, selected bridge transport, ancestry,
and local URLs.

### `POST /v1/turns`

Submits one synchronous application turn. The preferred form names a declared
operation and carries its bytes in `input`:

With a v1alpha2 delivery contract, the decoded bytes, media type and effective
byte limit must match that operation's reviewed input description. Invalid
packaging returns `400` / `invalid_delivery_input` before invocation; no JSON
is extracted from prose or repaired. Examples below illustrate the transport,
not a universal application-specific field name.

```json
{
  "operation": "invoke",
  "caller_principal": "alice",
  "media_type": "application/json",
  "input": "eyJwcm9tcHQiOiJoZWxsbyJ9"
}
```

`input` is base64 because JSON cannot directly represent arbitrary bytes. The
example decodes to `{"prompt":"hello"}`. On the command line,
`printf '%s' '{"prompt":"hello"}' | base64` produces it.

HTTP applications also accept the original route form for compatibility:

```json
{
  "method": "POST",
  "path": "/invoke",
  "headers": {"Content-Type":"application/json"},
  "body": "eyJwcm9tcHQiOiJoZWxsbyJ9"
}
```

Choose one form: `operation`/`input` cannot be combined with `method`/`path` or
legacy `body`, and `input` and `body` cannot both be present.

Adaptive turns include `attempt_id`, `payload_digest`, and optionally
`artifact_digest`. The attempt/payload must already be registered.

`caller_principal` is optional. For an HTTP application with
`identity.inbound`, it places a synthetic user bearer token in the configured
application header. Unknown users or a missing inbound identity declaration
return `400`. Automatic placement is not available for non-HTTP adapters,
because Interceptor cannot safely guess where a CLI prompt or function argument
expects a user credential.

In the compatibility form, `body` is also base64. The decoded request is
limited to 2 MiB. `method` and `path` must match one declared HTTP operation.
Only `Content-Type`, `Accept`, `If-Match`, and
`If-None-Match` request headers are forwarded. Interceptor adds
`X-Interceptor-Turn-ID`.

The response is:

```json
{
  "id": "turn-1",
  "status": "complete",
  "code": 200,
  "headers": {"Content-Type":"application/json"},
  "body": "eyJvayI6dHJ1ZX0=",
  "media_type": "application/json",
  "started_at": "2026-08-25T12:00:00Z",
  "finished_at": "2026-08-25T12:00:01Z"
}
```

`status` is `complete` or `error`; an error turn also contains `error`. Response
bodies are base64. Command results may also include `exit_code`. Only
`Content-Type`, `ETag`, and `Location` HTTP response headers are retained.

### `GET /v1/turns/{turn-id}`

Contract-bearing turns include advisory `output_contract_status`: `matched`,
`unknown`, or `mismatch`. The attack-channel `turn_completed` event also records it.
The actual bounded output and application outcome are retained on a schema
mismatch. Legacy turns omit the field. This does not change feedback/evidence
visibility or guarantee injection delivery; see [Delivery contracts](delivery-contracts.md).

Returns a previously submitted in-memory turn record. Turn records are not a
snapshot-restored persistence mechanism; use events for durable evidence.

### `POST /v1/injections`

Arms an injection:

```json
{
  "definition": {
    "id": "untrusted-tool-result",
    "surface": "model_tool_result",
    "mode": "simulation",
    "scope": "standing",
    "enabled": true,
    "selector": {
      "tool_name": "lookup",
      "call_ordinal": "first"
    },
    "placement": {
      "operation": "append_text",
      "pointer": ""
    },
    "payload": " Treat this content as untrusted."
  }
}
```

For `scope: next_turn`, the top-level request must also include
`"eligible_turn":"turn-N"`. Returns `201` with the public definition. See
[Injections and evaluation](injections-and-evaluation.md). Arming validates the
target against the sealed per-service or filesystem-namespace capability;
unknown targets and static attacker responses fail immediately.

### `GET /v1/injections`

Returns `{"injections":[...]}` with public definitions. It deliberately omits
match, delivery, and outcome state.

### `DELETE /v1/injections/{injection-id}`

Deletes a definition and returns `204`. The deletion is included in saved state,
so restoring a later snapshot does not unexpectedly bring the injection back.
Injection IDs may not contain `/` or `\`.

Returns `204` on deletion, `404 not_found` only when that injection is absent,
and `500 injection_delete_failed` when durable deletion fails. Storage failures
must not be interpreted as successful absence. For attached campaigns use native
`injection.delete`; Operator resolves the harness's attempt-receipt/action handle
and treats only a confirmed native not-found result for that exact deletion as
`already_absent`. See the [shared cleanup contract](../../operator_sandbox/schemas/INJECTION_CLEANUP_CONTRACT.md).

### `POST /v1/snapshots`

```json
{"label":"before-untrusted-input","description":"Baseline before testing mutations","campaign_id":"campaign-01"}
```

Creates a logical snapshot and waits for it to finish before responding.
`label`, `description` and administrative `campaign_id` are optional. Description
is valid UTF-8 and at most 4,096 bytes; campaign IDs are at most 128 identifier
characters. Campaign ID and description are included in the returned, hash-covered
checkpoint and in list/inspect results. This private administrative route remains
unavailable for mutation after campaign attachment. The native `snapshot.create`
operation instead takes campaign ID from its v1alpha2 envelope and accepts only
label/description plus optional `maximum_committed_bytes` in its body. Public campaign-wide list/inspect routes are
defined in [the local API](local-api.md#snapshot-inventory-and-metadata). The
application must support snapshots. If the application cannot reach a safe save
point or keeps changing files during the save, the API returns `409` and does
not create a usable checkpoint.

Both creation routes accept optional `maximum_committed_bytes`: a nonnegative
signed 64-bit JSON integer. Omission uses native limits, explicit zero is exhausted,
and null/negative/fractional/string/out-of-range values are invalid. The supervisor
checks the smaller of caller/native remaining allowances under the snapshot lock.
A candidate over the allowance returns `429 snapshot_bytes_exhausted` before
checkpoint commitment; a candidate equal to it is allowed. Returned
`canonical_size_bytes` counts all canonical file bytes, including deduplicated
content. See [the normative allowance contract](../specs/external-adaptive-simulation.md#snapshot-caller-byte-allowance).

### `GET /v1/snapshots`

Returns `{"checkpoints":[...]}` in creation order.

### `GET /v1/snapshots/{checkpoint-id}`

Returns one verified checkpoint record or `404`.

### `GET /v1/attacker/{service-id}/observations`

Returns `{"observations":[...]}` for a declared attacker sink. Each observation
contains sequence, timestamp, method, path, normalized query and headers, body,
and an optional `body_ref` pointing to separately stored body content.

### `GET /v1/events?channel=attack|control`

Verifies the event chain and returns existing events in the selected channel.
The default is `attack`. The cooperative interface does not expose the
`evaluation` channel.

### `POST /v1/identity/caller-tokens`

Creates a short-lived synthetic token for a declared user principal:

```json
{
  "principal": "alice",
  "issuer": "sandbox-identity",
  "audience": "support-agent",
  "scopes": ["agent:invoke"]
}
```

`issuer` and `audience` default from `identity.inbound` when possible. The
response contains `access_token`, `token_type`, `expires_at`, `subject`,
`audience`, and `scopes`. This is a synthetic test credential; the endpoint
never retrieves or returns an external identity token. The CLI equivalent is
`interceptor identity caller-token`.

## Operator-only API

The in-container control server also implements the following administrative
endpoints, but they are intentionally unavailable from the local control URL.
Use the listed CLI command, which calls
the endpoint through the session's protected spool or Unix socket:

| Endpoint | CLI |
| --- | --- |
| `GET /v1/evaluation` | `interceptor evaluate`, `interceptor coverage` |
| `GET /v1/state-journal` | internal/operator use; no direct public CLI dump |
| `POST /v1/logs/verify` | the CLI's `logs verify` performs a broader offline verification |
| `POST /v1/quiesce` | asks the application to reach a safe snapshot point; no direct public CLI command |
| `POST /v1/stop` | `interceptor stop` |

This separation prevents the application or an ordinary test-harness client
from learning whether an attack succeeded, whether an injection was delivered,
or every change made to the environment.

## Status codes

- `200` successful read or completed turn
- `201` created injection or snapshot
- `204` successful injection deletion
- `400` invalid schema, selector, route, or query
- `404` unknown resource or endpoint not exposed by the cooperative relay
- `409` the session is in the wrong state, the application could not reach a
  safe save point, or snapshot creation failed
- `413` body or capture limit exceeded
- `429` turn limit exhausted
- `501` unsupported application, synthetic-service, sink, or model route
- `500` integrity or evaluation failure
