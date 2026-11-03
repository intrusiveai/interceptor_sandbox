# Architecture

## Component view

At a high level, the application and all simulated services run inside one
isolated container. A small host process gives the container carefully limited
access to the real model provider and exposes local URLs to the tester.

```text
operator / harness
   |                         host
   |  CLI and loopback HTTP    |
   v                           v
+------------------+   +---------------------+
| interceptor CLI  |   | model connector     |
| session manager  |   | provider allowlist  |----> fixed model provider
+--------+---------+   +----------+----------+
         | spool files (default)  |
         | or Unix sockets        |
=========|========================|================ container boundary
         v                        v
  +---------------------------------------------------------------+
  | sandboxd supervisor                                            |
  |  +-------------+  +----------------+  +---------------------+ |
  |  | application |  | DNS + HTTP/TLS |  | model proxy         | |
  |  | non-root    |  | service plane  |  | records exchanges   | |
  |  +-------------+  +----------------+  +---------------------+ |
  |       | declared files, mutable roots, state journal, events   |
  +---------------------------------------------------------------+
```

For Operator integration, the network-disabled attack harness talks only to the
trusted Operator broker over inherited pipes. Operator calls the integrated
host service on `127.0.0.1:8080`, without integration authentication:

```text
Operator guest -> typed Operator pipe/broker -> localhost Interceptor API
               -> private spool/Unix bridge -> sandboxd
```

`interceptor run` owns both the service and target. The first attach binds a
campaign; the supervisor retains native v1alpha2 campaign access, closure, operation
records and budgets. The host service adds sequential restore/confirmed stop and
protected evidence download. No gateway process, lease or restart protocol is
needed. See [Local integration](local-api.md).

The same compiled Go program is both the host command-line tool and the
in-container supervisor. The wrapper image combines that program with the exact
approved application image. The supervisor starts the application, runs the
simulated services, records activity, and coordinates safe snapshot points.

Host file management applies a configurable 512 MiB default aggregate spool limit
with one-second checks and Docker shutdown on excess. Shutdown removes spools;
retained evidence and blobs belong to independently purgeable session groups. See
[file management](file-management.md) for configuration and cleanup rules.

## How the application becomes a sandbox image

There are two related container images:

1. The **application image** is built by the application developer. It contains
   the application and its normal runtime dependencies.
2. The **wrapper image** is built by `interceptor app build`. It starts from the
   application image, adds the Linux `interceptor` executable, and makes that
   executable the container entrypoint. When local TLS is enabled it also
   contains the generated public sandbox CA, but no TLS private key.

The build and startup sequence is:

```text
Application source
      │ docker build
      ▼
Application image
      │ interceptor app build
      │ adds /usr/local/bin/interceptor
      ▼
Wrapper image
      │ interceptor run
      ▼
interceptor sandboxd
      ├─ starts simulated services and the model proxy
      ├─ prepares the selected application adapter and checks readiness
      └─ opens the private application/control bridge
```

The Interceptor executable is one program with several modes. On the host it
acts as the CLI and session manager. In the wrapper container it runs in
`sandboxd` mode as the supervisor. It can also run a restricted helper used by
one-shot command interfaces and command-based snapshot preparation. A separate
`sandboxd` executable does not need to be installed in the application image.

`interceptor app build` does not start any services or listeners. It also does
not copy the Environment Blueprint, session data, model-provider credentials,
or TLS private keys into the image. It creates the reusable wrapper image and
records which exact application, Interceptor build, Sealed Environment, and
public TLS identity it belongs to.

When a code interface declares `adapter_asset`, the builder also copies that
sealed adapter directory into the application workdir. Invocation loads this
installed copy; the original application image remains unchanged.

When `interceptor run` starts a session, it verifies that wrapper record, mounts
the Sealed Environment and new session storage, and starts the wrapper with the
`sandboxd` argument. The resulting container command is conceptually:

```sh
/usr/local/bin/interceptor sandboxd
```

The application's original image entrypoint is therefore not used during a
sandbox session. Instead, the selected adapter uses
`application.launch.command` and `application.launch.workdir` from the
Blueprint. It may start that command once, use it as the base argv for each
call, or use it as the Python/Node runtime. This is why those fields must fully
describe the selected interface's executable.

## Which process creates each listener

The local interface is split across the host and container:

| Owner | What it starts |
| --- | --- |
| Application process | Its native interface: local HTTP, one-shot command, stdin/stdout or REPL stream, Python/Node module, or external adapter protocol. |
| `sandboxd` inside the container | The selected application-interface adapter, closed DNS resolver, HTTP/HTTPS hostname routers, simulated HTTP and MCP services, synthetic identity issuers, attacker sinks, the model proxy at `MODEL_BASE_URL`, and the private application/control bridge. |
| `interceptor run` on the host | Integrated localhost Operator API, target lifecycle, evidence export and fixed-provider model connector. |

Typed native operations cross the private bridge to `sandboxd`. Model requests travel in the other direction: the
application calls the in-container model proxy, which crosses the private bridge
to the host connector. The provider credential exists only on that host side.

## Runtime containment

Each session has one Docker container configured to limit what the application
can reach and how many host resources it can consume:

- `--network none`;
- a read-only root filesystem and bounded temporary filesystem;
- no-new-privileges;
- all capabilities dropped from the application;
- PID, memory, CPU, turn, token, wall-time, snapshot, and blob limits;
- access only to the approved environment files, recorded session data, and
  declared writable folders; and
- a loopback-only DNS resolver with no upstream forwarding, plus simulated
  service hostnames that resolve only inside the container.

When enabled, standard application DNS uses UDP/TCP 53 on loopback. Declared
names resolve to `127.0.0.1`; unknown names fail and create coverage evidence.
A namespaced low-port setting lets the unprivileged supervisor bind ports
53/80/443 without granting the application a network capability. HTTP routes
by exact `Host`; HTTPS terminates TLS 1.2+ and requires matching SNI and `Host`.

The supervisor initially uses only the limited capabilities required to set
identities and coordinate the application. The application runs as a non-root
user and cannot access the private bridge directories.

## Application-interface adapters

The supervisor has one adapter-neutral invocation contract: a declared
operation ID, bounded input bytes, media type, deadline, and result. Built-in
adapters translate that contract to HTTP, a process invocation, a persistent
byte stream, or a language function call. The external adapter uses the same
contract over newline-delimited JSON.

This separation keeps session concerns outside the adapters. The supervisor
continues to enforce turn and wall limits, wait for active calls before a
snapshot, record downstream model and simulated-service activity, and stop the
application process group. An adapter is responsible only for readiness,
invocation, cancellation behavior, and translating a result. Adding a future
interface therefore does not require a new host/container transport or control
API.

The integrated host API uses loopback HTTP under the trusted local-host
harness boundary. HTTP applications retain their native declared paths.
Non-HTTP applications receive synthesized `POST /operations/<id>` paths; that
HTTP request does not imply that the application itself contains a web server.

These controls reduce the chance that model-driven behavior can affect real
systems. Docker still shares the host's operating-system kernel, so it is not
the same security boundary as a separate virtual machine. See
[Security and trust model](security.md).

## Host/container transports

The host and container need a private way to exchange application, control, and
model requests. Interceptor calls this the **bridge transport**. Both available
transports carry the same requests; only the delivery mechanism differs.

### File spool: portable default

`--bridge-transport spool` passes each request and response through short-lived
files in a private session directory. A file becomes visible to the receiver
only after it is completely written, so the receiver never acts on a partial
request. Completed, canceled, and expired exchanges are cleaned up.

The implementation protects this exchange with:

- `0700` spool directories and `0600` frames;
- strict request formats, hard size limits, deadlines, and hard-to-guess IDs;
- cancellation markers so work can stop when the caller gives up;
- cleanup for files left behind by a crash or interruption;
- one-at-a-time ordering for administrative actions such as snapshots;
- at most eight application requests in progress at once; and
- no automatic retry when it is unclear whether a state-changing request ran.

The real model-provider credential is never placed in these files. It remains in
the host connector and is added only when the host contacts the provider.

Regular files cross Docker Desktop bind mounts reliably, which makes this the
portable macOS and Linux default. The spool is a transport mechanism, not the
state journal or snapshot format.

### Unix sockets: explicit Linux fast path

`--bridge-transport unix` uses operating-system communication endpoints called
Unix sockets instead of short-lived files. This can reduce overhead on a native
Linux host where Docker and the CLI share the same operating-system kernel.

Selection is always explicit. Interceptor does not detect, select, retry, or
fall back between transports. Do not use this mode with Docker Desktop for
macOS, a remote Docker daemon, or a filesystem projection that exposes a socket
pathname without the originating kernel endpoint. An unsuitable selection
fails the session closed.

## Local interfaces

The attached host process binds the application and cooperative control servers
to `127.0.0.1` only. Both are unauthenticated by design; processes running as
the same local OS user are in the MVP trust boundary.

The application bridge exposes only the operations approved in the Blueprint. The
control server exposes operations suitable for a cooperating test harness.
Sensitive administrative details—including final evaluation results and the
complete state-change history—remain available only through CLI commands over
the private bridge.

## Model data path

1. The application calls the Blueprint's local-only `MODEL_BASE_URL`.
2. The in-container proxy recognizes the supported native OpenAI/Azure,
   Anthropic Messages, Gemini/Vertex, Bedrock Converse, and LiteLLM routes. It
   rejects unsupported and streaming routes.
3. It applies matching tool-result injections using that protocol's own tool
   representation and stores larger recorded content separately.
4. A model frame crosses the selected private bridge.
5. The host connector checks that the request belongs to the current session,
   arrives in the expected order and format, stays within size limits, uses a
   model explicitly allowed by the operator, and matches the selected provider
   adapter.
6. The adapter keeps the request like-for-like: each native API goes to its
   matching provider operation; Converse uses the host AWS SDK and LiteLLM
   accepts either supported OpenAI-compatible route. HTTP adapters use a fixed
   `--provider-base-url` and provider-specific resource bindings.
7. The adapter adds host-only authentication from a configured credential
   store, workload identity, or development-only environment source. HTTP
   redirects to another origin are rejected.
8. The native response is recorded and returned in the same protocol the
   application used. The proxy records usage and confirmed injection delivery
   before returning it.

The application keeps its framework's native API, but the operator must choose
a compatible host provider. See [Model providers](model-providers.md) for the
supported pairings and their limits.

## Identity data path

Identity stays entirely inside the simulated environment:

1. At startup, `sandboxd` creates a fresh signing key for each declared issuer
   and gives the application any declared OAuth settings, projected token path,
   or AWS container-credential endpoint.
2. For autonomous work, the application obtains a short-lived token with its
   synthetic client credentials.
3. For delegated work, the harness names a synthetic user. Interceptor attaches
   a caller token to the HTTP application request, and the agent exchanges that
   token while identifying itself with its own synthetic client credentials.
4. A protected simulated HTTP or MCP service verifies the token and checks the
   current grant or delegation before handling the request.
5. Issuance and authorization decisions are recorded without saving raw tokens
   or secrets.

The host is not an identity proxy and never exchanges a synthetic token for a
real credential. This boundary is deliberate: Interceptor tests how the agent
handles identity and delegated authority within the fake world, not whether a
production identity provider accepts it. See [Synthetic identity and delegated
access](identity.md).

## State and evidence

The Sealed Environment is the unchanged starting point. Interceptor saves each
managed state change to an ordered journal before applying it. Observations go
to a separate event log. Both logs link each entry to the previous one so later
editing can be detected. Large request bodies and changed files are stored once
and referred to by a SHA-256 digital fingerprint.

Each snapshot records the exact position in those histories and the fingerprints
of the approved application, environment, and saved files. The journal includes
changes to identity grants, delegations, and issuer failure settings. Restore
creates a new session from the approved starting point and reapplies only
changes that were successfully recorded. See [Snapshots and
restore](snapshots-and-restore.md).
