# Local API reference

While `interceptor run` or `restore` is open, it prints two local-only base URLs:

```text
Session sess-... running
  application: http://127.0.0.1:<port>
  control: http://127.0.0.1:<port>
```

They do not require a password because they accept connections only from the
same machine (`127.0.0.1`). Do not publish them through a reverse proxy or port
forward. Any other process running under the same local user account should be
treated as able to use them.

All control request bodies use strict JSON: misspelled or unknown fields are
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

JSON cannot directly represent arbitrary file or binary content, so `body` is
encoded as base64 text. For example, the JSON body `{"prompt":"hello"}` becomes
`eyJwcm9tcHQiOiJoZWxsbyJ9`. Most language runtimes provide a base64 helper; on
the command line, `printf '%s' '{"prompt":"hello"}' | base64` produces it. The
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

Deletes a definition and returns `204`. The deletion is included in saved state,
so restoring a later snapshot does not unexpectedly bring the injection back.
Injection IDs may not contain `/` or `\`.

### `POST /v1/snapshots`

```json
{"label":"before-untrusted-input"}
```

Creates a logical snapshot and waits for it to finish before responding. The
application must support snapshots. If the application cannot reach a safe save
point or keeps changing files during the save, the API returns `409` and does
not create a usable checkpoint.

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
- `501` unsupported application, fixture, sink, or model route
- `500` integrity or evaluation failure
