# Adaptive simulation interface

Interceptor can be used by Operator Sandbox, AEF, or an independent harness.
The interface is producer-neutral: the external engine creates payload bytes
and decides what to try; Interceptor verifies artifacts, executes declared
operations, limits live feedback, and exports authoritative evidence.

## Session policy and discovery

An Environment Blueprint may declare:

```yaml
adaptive:
  feedback_profiles: [black-box, diagnostic]
  max_attempts: 200
  max_artifacts: 400
```

Defaults are `black-box`, 200 attempts, and 400 artifacts. Select one immutable
profile with `interceptor run --feedback-profile <profile>`. `black-box`
returns target output and operation state only; `diagnostic` can add coarse
allowlisted failures. `oracle-assisted` can add only the identity, type, and
positive outcome of a declared oracle when recorded evidence explicitly ties
that outcome to the requested attempt. Protected events, raw oracle internals,
canaries, fixtures, and provider-boundary details are never projected into a
view. An omitted oracle outcome is not evidence that it did not occur.

Generate the deterministic, secret-free manifest before a run:

```sh
interceptor capabilities --environment ./environment
```

During a run it is also available at `GET /v1/capabilities`. Its digest binds
the sealed Environment and Application, supported operations, per-service
HTTP/HTTPS transports and injection surfaces, dynamic/static attacker response modes, logical
file-injection namespaces and creation ceilings, the exact operations operation
set, and per-surface scopes, placements, selector fields, and carriers. It also
reports model codecs/adapters,
feedback profiles, snapshot support, DNS policy/query limits, HTTP/HTTPS front
door availability, TLS trust mode/minimum version, and limits. Credentials, credential-store configuration,
payloads, fixture content, private paths, and evidence are excluded.

## Artifact and attempt flow

The concrete flow is:

1. The harness creates payload bytes and computes the declared digest.
2. It posts `{descriptor, content}` to `POST /v1/artifacts`. JSON encodes
   `content` as base64. Interceptor canonicalizes only when the descriptor
   explicitly selects `jcs-v1`, verifies digest, size, and media type, and
   commits the object immutably.
3. It posts an `AttemptContext` to `POST /v1/attempts`. The payload must already
   be registered. Parent attempts must already exist in the same campaign and
   thread, and attempt indexes increase monotonically.
4. It invokes `POST /v1/turns` with `attempt_id`, `payload_digest`, and either
   inline input or `artifact_digest`. An armed injection may carry the same
   attempt and payload identity.
5. It collects native `observation.read` for the exact invocation turn ID, then
   reads permitted entry bytes with `observation.content.read`. The private GET
   view is also available for host administration. See [live feedback](feedback.md).

Exact registered content and target output referenced by an ObservationView are
available from `GET /v1/artifacts/{digest}` on the trusted local control API. A
operations caller needs the typed `artifact.read` operation.

Artifact and attempt registrations are journaled. A restore reconstructs only
registrations committed at the selected checkpoint, preserving lineage without
copying later attempts backward in time.

## Evidence

`interceptor logs export` now includes `artifacts.json`, `attempts.json`, and
`evidence-bundle.json` when present. The EvidenceBundle indexes attempts by
context and payload digest, correlated event sequences, snapshot digest and
canonical size, restore ancestry, final oracle/injection/coverage outcomes,
completeness bounds, sealed digests, and the capability-manifest digest. Raw
bodies remain content-addressed blobs. A missing final evaluation after an
abnormal shutdown is represented as partial completeness.

The live harness does not receive this protected bundle. Export remains a
trusted host operation available only after the session has stopped or failed.
Interceptor does not call AEF; Operator or another trusted collector verifies
and relays the bundle.

See [Local integration](local-api.md) for the same-host MVP integration
boundary and [Injections and evaluation](injections-and-evaluation.md) for the
new response-side surfaces and typed event oracle.
