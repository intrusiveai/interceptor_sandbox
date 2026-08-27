# Decision 0004: Modular application interfaces

Status: accepted

## Context

The original application contract required every test target to run a local
HTTP server and enumerate its routes. That fits web services but makes a CLI,
REPL, library-style agent, or framework entry point add an artificial server
solely for Interceptor. The adaptation belongs in the test harness when it can
be done without weakening containment or evidence.

## Decision

The Environment Blueprint declares `application.interface`, one interface
`kind`, and a non-empty allowlist of named `operations`. `sandboxd` owns an
adapter-neutral invocation contract and delegates application-specific mechanics
to one adapter.

The MVP includes:

- `http`: native allowlisted HTTP method/path mappings;
- `command`: a fresh application-user process per invocation, with sealed argv
  mappings, stdin policy, success statuses, bounded output, concurrency, and
  cancellation;
- `stdio` and `repl`: one persistent process using pipes or a Linux PTY and an
  explicit response-completion rule;
- `code`: an in-memory driver that imports and invokes a Python or Node module;
  and
- `external`: a fixed executable speaking
  `interceptor.application/v1` newline-delimited JSON.

The existing host application bridge remains HTTP. It forwards native paths for
the HTTP kind and synthesizes `POST /operations/{id}` for the other kinds. The
control turn API accepts named operations for every kind and retains the legacy
method/path request for HTTP compatibility.

Interface adapters do not own containment, model-provider access, evidence
stores, injections, state journals, or snapshot coordination. Active calls are
tracked by the supervisor, so quiescence blocks new calls and waits for current
ones before filesystem state is captured. Referenced external-adapter
configuration is sealed like every other Blueprint asset.

## Consequences

Applications keep their natural entry point, and new interfaces can implement
one bounded runtime contract without changing host transport or administrative
APIs. Configuration is more explicit because byte-stream completion and code
initialization cannot be inferred safely.

REPL calls are serialized. Full-screen terminal behavior is out of scope.
Python and Node calls share a long-running runtime, so a timeout cannot safely
preempt every form of user code; functions should be bounded. The external
protocol is deliberately small and does not support streaming or arbitrary
runtime plugins in the MVP.
