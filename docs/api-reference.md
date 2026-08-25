# Local API reference

While `interceptor run` or `restore` is attached, it prints two loopback base
URLs:

```text
Session sess-... running
  application: http://127.0.0.1:<port>
  control: http://127.0.0.1:<port>
```

They are intentionally unauthenticated and bind only to `127.0.0.1`. Do not
publish them through a reverse proxy or port forward. Local processes running as
the same OS user are within the MVP trust boundary.

All control request bodies are strict JSON: unknown fields and trailing values
are rejected. Control responses are JSON and include `Cache-Control: no-store`.
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

The application base URL accepts the application's native request and response
format. Only method/path pairs declared by `application.io.served_routes` are
forwarded; undeclared routes return `501`.

```sh
curl -sS -X POST "$APPLICATION_URL/invoke" \
  -H 'Content-Type: application/json' \
  --data '{"prompt":"hello"}'
```

Use this interface when an existing harness expects the application's natural
API. Use cooperative turns when you want a turn ID and the structured turn
envelope.

## Cooperative control endpoints

### `GET /v1/status`

Returns session metadata, including identity, phase, environment/application
digests, resource limits and consumption, selected bridge transport, ancestry,
and local URLs.

### `POST /v1/turns`

Submits one synchronous application turn:

```json
{
  "method": "POST",
  "path": "/invoke",
  "headers": {"Content-Type":"application/json"},
  "body": "eyJwcm9tcHQiOiJoZWxsbyJ9"
}
```

`body` is a byte array in the Go contract, so JSON represents it as base64. The
decoded request body is limited to 2 MiB. `method` and `path` must match a
declared served route. Only `Content-Type`, `Accept`, `If-Match`, and
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
  "started_at": "2026-08-25T12:00:00Z",
  "finished_at": "2026-08-25T12:00:01Z"
}
```

`status` is `complete` or `error`; an error turn also contains `error`. Response
bodies are base64 and bounded at 2 MiB. Only `Content-Type`, `ETag`, and
`Location` response headers are retained.

### `GET /v1/turns/{turn-id}`

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
[Injections and evaluation](injections-and-evaluation.md).

### `GET /v1/injections`

Returns `{"injections":[...]}` with public definitions. It deliberately omits
match, delivery, and outcome state.

### `DELETE /v1/injections/{injection-id}`

Deletes a definition and returns `204`. Deletion is journaled. Percent-encode
path content; injection IDs themselves may not contain `/` or `\`.

### `POST /v1/snapshots`

```json
{"label":"before-untrusted-input"}
```

Creates a logical snapshot synchronously and returns `201` with its checkpoint.
The application must declare snapshot capability. A failed quiescence or
unstable filesystem returns `409` and does not publish a checkpoint.

### `GET /v1/snapshots`

Returns `{"checkpoints":[...]}` in creation order.

### `GET /v1/snapshots/{checkpoint-id}`

Returns one verified checkpoint manifest or `404`.

### `GET /v1/attacker/{service-id}/observations`

Returns `{"observations":[...]}` for a declared attacker sink. Each observation
contains sequence, timestamp, method, path, normalized query and headers, body,
and an optional content-addressed `body_ref`.

### `GET /v1/events?channel=attack|control`

Verifies the event chain and returns existing events in the selected channel.
The default is `attack`. The cooperative interface does not expose the
`evaluation` channel.

## Operator-only API

The in-container control server also implements the following endpoints, but
the loopback control relay rejects them. Use the listed CLI command, which calls
the endpoint through the session's protected spool or Unix socket:

| Endpoint | CLI |
| --- | --- |
| `GET /v1/evaluation` | `interceptor evaluate`, `interceptor coverage` |
| `GET /v1/state-journal` | internal/operator use; no direct public CLI dump |
| `POST /v1/logs/verify` | the CLI's `logs verify` performs a broader offline verification |
| `POST /v1/quiesce` | snapshot coordination; no direct public CLI command |
| `POST /v1/stop` | `interceptor stop` |

This separation prevents application-facing or cooperative harness clients from
learning oracle verdicts, delivery state, or the full persistent mutation log.

## Status codes

- `200` successful read or completed turn
- `201` created injection or snapshot
- `204` successful injection deletion
- `400` invalid schema, selector, route, or query
- `404` unknown resource or endpoint not exposed by the cooperative relay
- `409` invalid lifecycle state, quiescence failure, or snapshot failure
- `413` body or capture limit exceeded
- `429` turn limit exhausted
- `501` unsupported application, fixture, sink, or model route
- `500` integrity or evaluation failure

