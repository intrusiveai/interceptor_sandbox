# Architecture

## Component view

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

The same compiled Go program is the host CLI and the in-container supervisor.
The wrapper image adds it to the digest-pinned application image. The
supervisor starts the application, owns synthetic services and recordings, and
coordinates quiescence and snapshots.

## Runtime containment

Each session has one Docker container configured with:

- `--network none`;
- a read-only root filesystem and bounded temporary filesystem;
- no-new-privileges;
- all capabilities dropped from the application;
- PID, memory, CPU, turn, token, wall-time, snapshot, and blob limits;
- only the Sealed Environment, session data, blob store, declared read-only
  seeds, and declared mutable roots mounted; and
- synthetic hostnames mapped to loopback services inside the container.

The supervisor initially uses only the limited capabilities required to set
identities and coordinate the application. The application runs as a non-root
user and cannot access the private bridge directories.

This narrows exposure but does not turn Docker into a VM-grade boundary. See
[Security and trust model](security.md).

## Host/container transports

All bridge transports carry the same logical HTTP or model messages. The
selected value is recorded in session metadata.

### File spool: portable default

`--bridge-transport spool` uses regular files in session-private directories.
A sender writes a bounded frame to a temporary file and atomically renames it
into a request directory. The receiver claims it, produces a response the same
way, and cleans the completed exchange.

The implementation includes:

- `0700` spool directories and `0600` frames;
- strict frame schemas, unpredictable request IDs, size bounds, and deadlines;
- cancellation markers so abandoned work can stop;
- orphan and expired-artifact cleanup;
- stable, serialized control request claiming;
- application request concurrency bounded at eight; and
- no automatic replay of ambiguous mutations.

The model provider credential is never placed in a frame. It exists only in the
host connector process and is added to the outgoing provider request there.

Regular files cross Docker Desktop bind mounts reliably, which makes this the
portable macOS and Linux default. The spool is a transport mechanism, not the
state journal or snapshot format.

### Unix sockets: explicit Linux fast path

`--bridge-transport unix` puts native Unix sockets in the bind-mounted session
directory. It may reduce file publication and polling overhead when the Docker
daemon, CLI, and bind mount share the same Linux kernel.

Selection is always explicit. Interceptor does not detect, select, retry, or
fall back between transports. Do not use this mode with Docker Desktop for
macOS, a remote Docker daemon, or a filesystem projection that exposes a socket
pathname without the originating kernel endpoint. An unsuitable selection
fails the session closed.

## Local interfaces

The attached host process binds the application and cooperative control servers
to `127.0.0.1` only. Both are unauthenticated by design; processes running as
the same local OS user are in the MVP trust boundary.

The application server exposes only `application.io.served_routes`. The control
server exposes a deliberate subset of the internal API. Evaluation, full
journal access, log verification, quiesce, and stop remain operator-only and are
reached through CLI commands over the protected bridge.

## Model data path

1. The application calls the Blueprint's loopback `MODEL_BASE_URL`.
2. The in-container proxy accepts only OpenAI-compatible chat completions.
3. It applies any matching transit injection and records content in the blob
   store.
4. A model frame crosses the selected private bridge.
5. The host connector verifies session identity, monotonically increasing
   request ID, codec, body limit, and model allowlist.
6. It sends the request to the fixed `--provider-base-url`, adds the host-only
   Bearer credential, and rejects cross-origin redirects.
7. The response returns through the proxy, where usage and confirmed delivery
   are recorded, then reaches the application unchanged.

## State and evidence

The Sealed Environment is immutable base state. Managed mutations are committed
to a write-ahead, hash-chained state journal. Observations go to a separate
hash-chained event log with attack, control, and evaluation channels. Large
bodies and changed file content are addressed by SHA-256 in a shared blob store.

Logical snapshots pin a journal position, file-manifest digest, event position,
environment digest, and application digest. Restore materializes a new session
from the sealed base and replays only committed state. See [Snapshots and
restore](snapshots-and-restore.md).

