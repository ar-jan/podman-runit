# podman-runit

A small, dependency-free Go scheduler for **stock rootless Podman 6.1.4**, with
Void Linux runit/turnstile user services. MIT licensed. This is a local project;
`podman-runit` is a local module identifier, not a published Go repository.

Install the companion package, then enable the user services once. Ordinary
image and Compose health checks are discovered automatically, without labels.
Podman runs the probes and owns timeouts, retries, health logs, grace periods,
and failure actions. The scheduler never runs probe commands or a periodic
Podman CLI scan. Docker Compose remains the provider for `podman compose`.

```sh
make check
make install DESTDIR="$PWD/build/stage"
# After installing the companion package on a stock-Podman system:
podman-runit enable --dry-run
podman-runit enable
podman-runit status
podman-runit doctor
podman-runit disable
```

See [Void setup and packaging](README.voidlinux.md) and
[validation evidence](tests/integration/REPORT.md). Installing or building this
project does not activate services. Legacy `podman-runit-healthcheck` cleanup,
package migration, and rollback are deliberately outside this implementation.

## Compatibility

| Feature | Policy |
| --- | --- |
| Stock native API 6.1.4, rootless | Tested; other native versions rejected pending validation |
| Image / CLI / Docker Compose health checks | Automatic, including CMD and CMD-SHELL |
| `start_period` | Probes run promptly; Podman applies failure grace |
| Compose `start_interval` | Stock 6.1.4 discards this in compatibility creation; cannot reproduce it |
| Podman startup probes | Excluded, with log and doctor/status diagnostic; multi-success transition is not exposed |
| Disabled automatic interval | Excluded; stock inspect omits zero intervals, stores default 30s explicitly |
| Subsecond intervals | Preserved as integer nanoseconds |
| Exclusion | Set `io.podman.runit.healthcheck=false` |
| Rootful / TCP / systemd / per-container services | Outside scope |

The API version comes from `Libpod-API-Version` or the Podman Engine component;
the top-level Docker `ApiVersion` is never used for native routes. An enabled
`file` or `journald` event backend is required. `events_logger=none` leaves the
scheduler waiting with an actionable error; there is no hidden polling fallback.
The file event backend itself polls internally (250ms in the inspected engine).

## Scheduling and recovery

One event loop owns a deadline heap and all scheduling state. Four workers by
default execute checks; each container has at most one outstanding request.
There is no buffered execution backlog: waiting work is one removable heap
entry per eligible container. Metadata and streaming have separate connections
from probes. Events are filtered to lifecycle changes, debounced by ID, and
always followed by authoritative inspection. Exec/health-status events do not
cause inspection loops. Periodic reconciliation defaults to 60 seconds and
refreshes all configurations. Failed snapshots never erase known containers.

The event subscription returns flushed response headers before the initial
snapshot, with a two-second timestamp overlap. The bounded 256-event buffer
triggers full reconciliation on overflow. Reconnect uses 250ms–30s exponential
backoff with jitter, then a complete snapshot before dispatch resumes. Idle
streams have no read deadline. Metadata has a 10s request deadline and a 16MiB
response cap; event records are capped at 1MiB. Probe requests have fresh Unix
connections and no redirects, preventing Go's retry of a side-effecting GET on
a reused connection.

Initial probes run promptly, including within `start_period`. Subsequent
probes run at completion plus interval, without catch-up bursts. Restart
reconstruction uses the newest health-log completion belonging to the current
`StartedAt`. Missing, external, capped or unusable history means one immediate
probe, subject to the recovery journal. Wall timestamps are converted into new
runtime deadlines. Restart/configuration changes replace queued entries;
old completions cannot restore old configuration.

An endpoint-specific kernel lock enforces a single cooperating scheduler.
Locks are keyed by the cleaned socket path, **not engine identity**: different
socket aliases or unrelated schedulers can evade exclusion. Use one owner per
engine. `doctor` reports lock/service status but cannot prove there are no other
implementations running.

Before dispatch, a private runtime journal records an uncertainty hold of
`2 × (probe timeout + 30s) + interval`. Successful responses clear the hold;
transport/API errors require reconciliation and defer another attempt by at
least one request budget. The journal survives a daemon crash. A fresh inspect
is performed before retry; success of the HTTP call never substitutes for
Podman's cumulative health state. Recovery holds remain conservative even if a
new health-log completion is already visible.

An omitted timeout uses 30s. Zero/unlimited timeout uses a five-minute client
budget plus 30s overhead; a server-side unlimited command may outlive that
budget. Timeouts over 24h are visibly excluded. There is no exactly-once or
cross-crash no-overlap guarantee: the API exposes neither idempotency tokens nor
a reliable in-flight marker. Avoid unlimited probes if that ambiguity matters.

SIGTERM stops dispatch and discovery, then drains for eight seconds by default.
After that it cancels requests and retains the journal; cancellation does not
kill server-side probes. Use `sv -w 15 down` for a normal stop. An immediate
kill/restart uses the same recovery holds. The status file distinguishes
waiting, synchronizing, ready, degraded, stopping and stopped, with PID and a
10s heartbeat. `status`/runit checks reject stale or dead-process readiness.
Unsupported containers are excluded individually and remain visible in status.

## Configuration

Both services resolve the same Unix socket; `DOCKER_HOST` has no effect on the
scheduler. Precedence is inherited environment, turnstile envdir, shared
`$XDG_CONFIG_HOME/podman-runit/common.conf` (default `~/.config`), service-local
`conf`, then explicit binary flags. Files are trusted POSIX shell assignments,
owned by the user and not group/other-writable. Assignments are exported.

```sh
# ~/.config/podman-runit/common.conf
PODMAN_RUNIT_SOCKET="$XDG_RUNTIME_DIR/podman/podman.sock"
```

Service-specific files cannot override the shared socket. `XDG_RUNTIME_DIR`
must be a user-owned private directory; no `/tmp` or rootful fallback exists.
`podman-healthcheckd --help` describes worker, reconcile, drain and socket flags.
Service-local `conf` can set `PODMAN_RUNIT_WORKERS`, `PODMAN_RUNIT_RECONCILE`
and `PODMAN_RUNIT_DRAIN`. The API launcher holds a separate endpoint lock across exec of `/usr/bin/podman`.
It refuses live/foreign sockets. It can remove only a same-user socket in a
private directory whose connection was refused and whose inode is unchanged,
allowing restart after an API crash. No setup command unlinks a socket.

The logger uses `${XDG_STATE_HOME:-$HOME/.local/state}/svlog/<service>`; its
initial config retains five 1MiB files. Existing log config is preserved.
Successes and normal failing probes are quiet. Infrastructure errors are rate
limited; unsupported configuration changes receive one diagnostic.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
make check
# Explicit stock binary, separate disposable storage, never the live socket:
python3 tests/integration/run.py --podman /path/to/stock/podman --keep
```

The integration suite needs rootless container support, curl, runit, stock
Podman 6.1.4 and Docker Compose. It pulls BusyBox 1.37.0 into isolated storage,
creates only test containers, and cleans up that store's containers. An explicit
`--keep` retains its temporary directory for inspecting failure evidence.
The status file also exposes cumulative metadata, event-stream and probe request
counts and maximum scheduling delay (nanoseconds), without logging each check.

Tests simulate scheduler clock advancement without sleeps for timing semantics,
and use Unix HTTP servers for request behavior, workers and shutdown.
