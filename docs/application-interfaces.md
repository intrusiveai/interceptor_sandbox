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
as a server or CLI. The application image must contain the runtime, application,
and its dependencies. An invocation adapter can either be installed in that
image or supplied separately with the Blueprint. The launch command identifies
the interpreter:

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
turn. An invocation exception fails that turn. A timeout or cancellation after
dispatch terminates the language process and makes the session unusable, because
the call may already have changed application state. Start a new session; do not
retry side-effecting calls without checking their outcome. Canceling a request
that is still waiting for an invocation slot does not terminate the session.

### Supplying an adapter separately

Use `code.adapter_asset` when the image does not already provide the configured
calling convention. It names a directory relative to the Blueprint, not the
shell's working directory:

```text
environment/
  environment.yaml
  assets/adapter/
    support_adapter.py
    helpers.py
```

```yaml
code:
  language: python
  adapter_asset: assets/adapter
  module: support_adapter
  initialize: initialize
  invoke: invoke
  close: close
  init_arguments:
    customer_id: alice
  concurrency: 1
```

The initializer receives one dictionary/object and returns an object with the
configured invocation method. That object persists across turns in one sandbox.
It can create an application instance and a customer session, then translate
each input into the application's own calling convention. Separate sandbox
instances get separate objects. In-memory state is not restored by filesystem
snapshots; leave `snapshot_capable: false` unless the application or a configured
state adapter implements a verified restore mechanism.

Sealing covers the adapter directory and helper files. `interceptor app build`
copies the verified code into `<launch.workdir>/.interceptor-adapter/` in the
wrapper. The original image stays unchanged. After editing an adapter, reseal
the Blueprint and rebuild the wrapper. No pip/npm installation or adapter code
execution occurs during the build. The released CLI needs Docker and its normal
release files, including the packaged Linux companion on macOS; it does not
need a host Python/Node interpreter, Go, or an Interceptor source checkout.

Python `module` is a dotted name inside the adapter directory. Packages require
`__init__.py`. Helpers can be imported from that directory; application imports
resolve from the configured working directory and installed Python packages.
Async initialization, invocation, and cleanup share one event loop.

For Node, set `language: node`, launch the image's Node executable, and use an
explicit entry file such as `module: ./support_adapter.mjs`. Relative imports
resolve inside the adapter; ordinary package imports can find the application's
`node_modules` in the parent working directory. Use `.mjs`, `.cjs`, or `.js`
with a suitable `package.json` in the adapter directory. For an application file
outside the adapter, use its explicit container path, for example
`import {SupportApplication} from '/app/support_app.mjs'`. Node retains `this`
when calling a method on the initialized object.

Optional `close` names a cleanup method on that object (or the imported module
if no initializer is configured). Interceptor awaits it on orderly shutdown,
with a two-second limit. Exceptions and cleanup timeouts are reported; forced
termination cannot guarantee cleanup. Application print/console output goes to
application logs independently of the response protocol.

Missing imports, dependencies, or methods fail startup with the failing stage.
Adapter paths cannot escape the Blueprint or contain symbolic/hard links. The
installed destination must be absent from the target image and cannot overlap
writable roots, filesystem seeds, image `VOLUME` paths, or runtime mounts such as `/tmp` and `/run`.
Use a dedicated application workdir such as `/app`. Adapters run with the
application's unprivileged identity and cannot modify their installed code.
Keep real credentials out of adapter files and `init_arguments`.

See the runnable [Python and Node adapter examples](../examples/invocation-adapters/README.md).

### Saving and restoring application state

A separately supplied invocation adapter can also implement application-specific
state persistence. The target application does not need a framework checkpointer
or source changes when the adapter can reconstruct all necessary state. Add this
mapping under `application.interface.code`:

```yaml
state:
  save: save_state
  restore: restore_state
  path: /app-state/agent.json
```

Declare `/app-state` in `application.persistence.mutable_roots`, enable
`snapshot_capable`, and use concurrency `1`. Choose a dedicated state directory
outside the application workdir, seeded assets, and file-injection namespaces.
`save_state()` returns a JSON object; `restore_state(state)` applies it to the
same object used by `invoke`. Both Python and Node support sync and async methods.
Without an initializer the methods resolve on the imported module.

On snapshot creation, Interceptor waits for invocations to complete, performs
configured quiescence, calls the save method, and includes the resulting file in
the normal verified filesystem snapshot. `quiesce.kind: none` does not skip the
save method. Saving occurs at snapshot boundaries, not on every turn. The driver
writes a versioned JSON document atomically and enforces `limits.max_blob_bytes`;
the file also counts against snapshot byte limits. It rejects lossy serialization,
including unsupported objects, cycles, non-finite numbers, and dropped fields.
Encode framework message objects explicitly; do not use pickle.

On restore, the driver initializes the application, loads the selected snapshot's
state, and awaits the restore method before reporting readiness. A fresh session
never loads a file merely because it exists. Missing, invalid, incompatible, or
rejected restored state fails startup; it never falls back to empty history.
Save errors fail the snapshot without replacing it with stale state. A save
that times out and may still be running makes that application session unusable.
State capture and admission waiting use the quiescence timeout (default five
seconds); restored startup uses the existing startup timeout.

The adapter must preserve messages and tool history, summaries, logical identity,
and any safeguards affecting future decisions. It must rebuild transient locks
and clients without replaying model requests or business actions. Background
state mutation must stop at the snapshot boundary through existing quiescence
facilities; serializing invocations alone does not stop background tasks. Save
must leave the live application logically unchanged and be safe to repeat.

This adapter is part of the tested configuration. It does not give the original
application production restart support, capture process memory, or recover
provider-side conversation state. See the [runnable examples](../examples/application-state-snapshots/README.md)
and [snapshot behavior](snapshots-and-restore.md).

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

### Applications that embed during initialization

A Python or Node invocation adapter can call the local model proxy during its
initializer and later turns. Configure both generation and embeddings SDK clients
with `http://127.0.0.1:18080/v1` and declared synthetic credentials. Enable the
independent host embeddings options described in [Model providers](model-providers.md#remote-embeddings).
Package SDK dependencies and any tokenizer cache in the target image; no runtime
downloads are available in the isolated container. The adapter can be supplied
separately through the existing sealed adapter asset.

Embedding calls preserve their model usage and vector metadata in protected
evidence. They do not confirm generation injection delivery. See the
[remote embeddings example](../examples/remote-embeddings/README.md) for startup
batch embedding and query embedding during later invocations.
