# Host lifetime and macOS sleep prevention

`interceptor run` owns the target and its local API. On macOS it runs as the
logged-in Docker Desktop user and automatically starts:

```text
/usr/bin/caffeinate -i -w <interceptor-host-process-pid>
```

No administrator power-setting changes or extra package are required. Interceptor
confirms the helper's `PreventUserIdleSystemSleep` assertion before launching the
target. It holds that assertion throughout execution, snapshot restore (including
the interval between source and replacement), and shutdown cleanup. Once cleanup
finishes, it stops/reaps the helper even if the local API remains open for evidence.
The PID watch releases the assertion if the host dies. Each Operator process owns
its own helper; neither component depends on the other's sleep prevention.

Display sleep and screen lock remain allowed. Automatic idle system sleep is
inhibited; explicit sleep, closing the lid, logout, shutdown and emergency sleep
are not prevented. Campaigns are supported while logged in and awake.

## Interruptions

The host checks macOS kernel sleep/wake timestamps every 500 ms while active and
on API admission. A changed timestamp closes execution on detection after wake;
this does not promise zero container execution before the host is scheduled again.
Unexpected helper exit and unreadable power state also close execution. Failed
initial verification rejects startup before launching the target.

SIGTERM (including logout) and SIGHUP carry `HOST_SERVICE_INTERRUPTED`. Changed
sleep state records `HOST_SLEEP_INTERRUPTED`; helper loss records
`SLEEP_PREVENTION_LOST`; unreadable state records `HOST_POWER_STATE_UNAVAILABLE`.
Docker event-stream disconnect records `DOCKER_CONNECTION_LOST`, failed container
inspection records `DOCKER_UNAVAILABLE`, and unexpected target exit records
`CONTAINER_INTERRUPTED`. These are terminal, even if Docker becomes available again.

The host closes API admission, cancels work, independently requests Docker kill,
and attempts to retain `host-interruption.json` plus the session termination
reason. Report unknown outcomes for interrupted operations. Failure to persist
must not delay termination. Confirm removal before deleting spools; retain journals
and artifacts. Unavailable Docker leaves cleanup explicitly unconfirmed. These
small host failure records are included in `logs export` when present.

Ctrl+C requests normal service shutdown. `session.stop` stops execution and its
helper while leaving the API available for retained evidence. Neither healthy
restore nor rejected restore preflight restarts the host/helper. Loss of the host
is terminal: later administration cleans up and exports evidence, never adopts or
resumes execution. Containers have no automatic restart policy.

## Optional per-user LaunchAgent

An administrator may run the same `interceptor run` command in a per-user launchd
LaunchAgent under the Docker Desktop user's account. Use absolute executable,
environment and data paths, the same Docker endpoint as the administrator CLI,
and private log/configuration directories. Set `RunAtLoad` and `KeepAlive` to
`false`; explicitly start a campaign only after Docker Desktop is available.
Logout ends the service; login must not restart a previous campaign. No system-wide
service, root helper or power-preference change is needed.

Linux runs the existing foreground/service lifecycle without a caffeinate helper.
The Docker interruption, signal, terminal closure and cleanup rules apply to both
platforms. See [validation](host-lifetime-validation.md) for exercised behavior.
