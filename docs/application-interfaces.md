# Application interfaces

Interceptor can test an application through HTTP, a one-command-per-request
CLI, a long-running stdin/stdout process, an interactive REPL, a small adapter
program, or direct Python/Node module invocation. The application does not have
to add an HTTP server solely for Interceptor.

The Environment Blueprint names the operations a tester is allowed to call and
selects one interface `kind`. `sandboxd` handles the shared work—timeouts,
concurrency limits, evidence, snapshot coordination, and the private host
bridge—while a small interface adapter handles the application-specific call.
This boundary is also how another interface can be added later.

Every non-HTTP interface appears at the printed application URL as:

```text
POST /operations/<operation-id>
```

The request body becomes the operation input and the adapter's output becomes
the response body. The control API can call the same operation by posting
`operation` and base64-encoded `input` to `/v1/turns`.

## HTTP

Use `http` when the application already serves an HTTP API:

```yaml
interface:
  kind: http
  operations:
    - id: invoke
      method: POST
      path: /api/agent/invoke
  http:
    listen: http://127.0.0.1:8080
    health_path: /healthz
  startup_timeout_ms: 30000
  turn_timeout_ms: 120000
```

The launch command runs once. Interceptor waits for an optional 2xx health
response and forwards only declared method/path pairs. The printed application
URL preserves the application's native paths and request format. Cooperative
turns may name `operation: invoke`; the older method/path turn form remains
supported for HTTP applications.

## Command

Use `command` for a CLI that accepts one independent invocation at a time:

```yaml
launch:
  command: ["/app/agent-cli"]
  workdir: /app
  environment:
    MODEL_BASE_URL: http://127.0.0.1:18080/v1
interface:
  kind: command
  operations:
    - id: ask
      arguments:
        - literal: --prompt
        - from: /prompt
  command:
    stdin: json
    output_media_type: application/json
    success_exit_codes: [0]
    concurrency: 2
  startup_timeout_ms: 30000
  turn_timeout_ms: 120000
```

For each turn, Interceptor starts the launch command as the unprivileged
application user. `arguments` appends fixed strings or values selected from the
JSON input with an RFC 6901 JSON Pointer. `stdin` is `raw`, `json`, or `none`.
Standard output is the result; standard error goes to the application log. A
turn is canceled by terminating only its command process group. The helper that
starts commands stays inside the container and accepts only the sealed command
and operation configuration.

Command applications have no long-running application process. Snapshot
quiescence must therefore be `command` or `none`.

## Stdio and REPL

Use `stdio` for a long-running, line- or delimiter-oriented process. Use `repl`
for an interactive program that expects a terminal:

```yaml
interface:
  kind: repl
  operations:
    - id: ask
  stdio:
    transport: pty
    input_suffix: "\n"
    ready_pattern: 'agent> $'
    response_mode: prompt
    terminal_pattern: 'agent> $'
    strip_echo: true
    strip_terminal: true
    output_media_type: text/plain
  startup_timeout_ms: 30000
  turn_timeout_ms: 120000
```

The launch command runs once. `pipes` connects ordinary stdin/stdout; `pty`
gives the process a Linux pseudo-terminal, which is needed by many REPLs for
prompting and flushing. Calls are intentionally serialized because one shared
byte stream cannot safely carry overlapping conversations.

Readiness can be immediate or wait for `ready_pattern`. A response ends using
one declared rule:

| `response_mode` | Completion rule |
| --- | --- |
| `line` | First newline. |
| `delimiter` | Exact `delimiter` text appears. |
| `prompt` | `terminal_pattern` matches at the end of collected output. |
| `idle` | Some output arrives and then no more arrives for `idle_ms`. |

`idle` is convenient but less deterministic than a prompt or delimiter. A
program that pauses while producing a response can be cut off early, so prefer
an explicit terminal marker where possible. `strip_echo` removes the submitted
input when a terminal echoes it; `strip_terminal` removes the completion marker
from the returned response.

Full-screen terminal applications, cursor control, resize events, and multiple
simultaneous REPL conversations are not supported.

## Python and Node modules

Use `code` when the application is naturally invoked as a function rather than
as a server or CLI. The application image must contain the runtime, module, and
all dependencies. The launch command identifies the interpreter, not an
application server:

```yaml
launch:
  command: ["/usr/bin/python3"]
  workdir: /app
  environment:
    MODEL_BASE_URL: http://127.0.0.1:18080/v1
interface:
  kind: code
  operations:
    - id: ask
  code:
    language: python
    module: my_agent
    initialize: create_agent
    invoke: invoke
    input_mode: single
    init_arguments:
      profile: test
    concurrency: 1
  startup_timeout_ms: 30000
  turn_timeout_ms: 120000
```

Interceptor starts a small in-memory driver with the configured Python or Node
executable. It imports the module once, optionally calls `initialize`, and then
calls `invoke` for each operation. Sync and async functions are supported.
`single` passes the decoded JSON value as one argument. Python also supports
`kwargs`, which requires a JSON object and expands its keys as named arguments.
An initializer receives `init_arguments` as one dictionary/object.

Python calls are serialized (`concurrency` is omitted or `1`). Node may allow
up to 32 overlapping async calls when the application code is safe to run that
way.

The return value must be JSON-serializable. Python values that are not
serializable fall back to their string form; Node serialization errors fail the
turn. Cancellation stops waiting for a result, but arbitrary user code cannot
always be safely interrupted inside a shared language process. Keep individual
functions bounded and rely on `turn_timeout_ms` as the outer limit.

## External adapter protocol

Use `external` when a small application-specific adapter is clearer than one of
the built-in mappings. The launch command is the adapter executable:

```yaml
interface:
  kind: external
  operations:
    - id: ask
  external:
    protocol: interceptor.application/v1
    config:
      mode: test
    config_asset: assets/adapter.yaml
    concurrency: 4
  startup_timeout_ms: 30000
  turn_timeout_ms: 120000
```

The protocol is newline-delimited JSON on stdin/stdout. Standard output is
reserved for frames; diagnostics belong on standard error. Interceptor first
sends:

```json
{"protocol":"interceptor.application/v1","type":"hello","config":{"mode":"test"}}
```

If `config_asset` is present, its sealed bytes are included as
`config_asset_base64` and its relative name as `asset_path`. The adapter replies
with `{"protocol":"interceptor.application/v1","type":"ready"}`. A call and
result look like:

```json
{"protocol":"interceptor.application/v1","type":"invoke","id":"turn-1","operation":"ask","input":{"prompt":"hello"},"media_type":"application/json"}
{"protocol":"interceptor.application/v1","type":"result","id":"turn-1","output":{"answer":"hi"},"media_type":"application/json"}
```

On cancellation Interceptor sends a `cancel` frame with the same `id`. The
adapter must correlate concurrent calls by ID and should stop unnecessary work
when it receives that frame. Streaming results, binary frames, adapter-initiated
calls, runtime plugin loading, and protocol negotiation beyond version 1 are
not supported in the MVP.

## Choosing an interface

Prefer the application's existing stable boundary: HTTP for an HTTP service,
command for independent CLI calls, REPL only for a truly interactive process,
and code for a well-defined module API. Use an external adapter when protocol
framing or initialization cannot be expressed by the built-ins. The narrowest
stable boundary usually produces the clearest evidence and the most reliable
snapshot tests.
