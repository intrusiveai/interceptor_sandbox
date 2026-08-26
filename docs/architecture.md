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
  |  | application |  | synthetic HTTP |  | model proxy         | |
  |  | non-root    |  | services/sinks |  | records exchanges   | |
  |  +-------------+  +----------------+  +---------------------+ |
  |       | declared files, mutable roots, state journal, events   |
  +---------------------------------------------------------------+
```

The same compiled Go program is both the host command-line tool and the
in-container supervisor. The wrapper image combines that program with the exact
approved application image. The supervisor starts the application, runs the
simulated services, records activity, and coordinates safe snapshot points.

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
- simulated service hostnames that resolve only inside the container.

The supervisor initially uses only the limited capabilities required to set
identities and coordinate the application. The application runs as a non-root
user and cannot access the private bridge directories.

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

The application server exposes only the routes approved in the Blueprint. The
control server exposes operations suitable for a cooperating test harness.
Sensitive administrative details—including final evaluation results and the
complete state-change history—remain available only through CLI commands over
the private bridge.

## Model data path

1. The application calls the Blueprint's local-only `MODEL_BASE_URL`.
2. The in-container proxy accepts only OpenAI-compatible chat completions.
3. It applies any matching injection to the request and stores larger recorded
   content separately.
4. A model frame crosses the selected private bridge.
5. The host connector checks that the request belongs to the current session,
   arrives in the expected order and format, stays within size limits, and uses
   a model explicitly allowed by the operator.
6. It sends the request to the fixed `--provider-base-url`, adds the host-only
   Bearer credential, and rejects cross-origin redirects.
7. The response returns through the proxy, where usage and confirmed delivery
   are recorded, then reaches the application unchanged.

## State and evidence

The Sealed Environment is the unchanged starting point. Interceptor saves each
managed state change to an ordered journal before applying it. Observations go
to a separate event log. Both logs link each entry to the previous one so later
editing can be detected. Large request bodies and changed files are stored once
and referred to by a SHA-256 digital fingerprint.

Each snapshot records the exact position in those histories and the fingerprints
of the approved application, environment, and saved files. Restore creates a new
session from the approved starting point and reapplies only changes that were
successfully recorded. See [Snapshots and
restore](snapshots-and-restore.md).
