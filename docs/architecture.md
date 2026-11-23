# Compatibility, Scheduling, and Recovery

## Compatibility

| Feature | Policy |
| --- | --- |
| Rootless Podman 6.x native API, 6.1.4 or later | Supported; other major versions are refused until validated |
| Image / CLI / Docker Compose health checks | Automatic, including `CMD` and `CMD-SHELL` |
| `start_period` | Probes run promptly; Podman applies failure grace |
| Compose `start_interval` | Discarded by Podman compatibility creation; not reproduced |
| Podman startup probes | Excluded with a log and `doctor`/`status` diagnostic (multi-success transition is not exposed) |
| Disabled automatic interval | Excluded; Podman inspect omits zero intervals and stores the default `30s` explicitly |
| Subsecond intervals | Preserved as integer nanoseconds |
| Per-container opt-out | Set label `io.podman.runit.healthcheck=false` |
| Rootful / TCP / systemd / per-container services | Out of scope |

The native API version is read from `Libpod-API-Version` or the Podman Engine
component; the top-level Docker `ApiVersion` is never used for native routes.

An enabled `file` or `journald` event backend is required. With
`events_logger=none`, the scheduler waits and reports an actionable error
rather than falling back to hidden polling. (Note that Podman's `file` event
backend polls internally.)

## Scheduling

One event loop owns a deadline heap and all scheduling state. Four workers by
default execute probes concurrently, with at most one outstanding request per
container. Waiting work is stored as a single removable heap entry per eligible
container rather than an unbounded queue.

Metadata/event streaming and probes use separate connections:

- **Events:** Filtered to container lifecycle actions, debounced by container
  ID, and always followed by authoritative container inspection. `exec` and
  `health_status` events do not trigger inspection loops.
- **Subscription and snapshots:** The event stream flushes response headers
  before the initial snapshot is taken, with a 2-second timestamp overlap. A
  bounded 256-event buffer triggers full reconciliation on overflow. Failed
  snapshots never erase known containers.
- **Reconnects:** Exponential backoff (250 ms–30 s with jitter), followed by a
  full snapshot before dispatch resumes.
- **Reconciliation:** Periodic full reconciliation runs every 60 seconds by
  default and refreshes all container configurations.
- **Limits and transport:** Idle event streams have no read deadline. Metadata
  requests have a 10 s deadline and 16 MiB cap; event records are capped at
  1 MiB. Probe requests use fresh Unix connections with redirects disabled to
  prevent Go from retrying a side-effecting `GET` on a reused connection.

### Timing

Initial probes run promptly, including during `start_period`. Subsequent probes
are scheduled at completion time plus interval, without catch-up bursts.

On restart, timing is reconstructed from the newest health-log completion
belonging to the current `StartedAt`. Missing, external, capped, or unusable
history schedules one immediate probe, subject to the recovery journal. Wall
timestamps are converted into monotonic runtime deadlines. Restarts and
configuration changes replace queued entries; old completions cannot reinstate
old configuration.

## Single-owner locking

An endpoint-specific kernel lock (`flock`) enforces a single cooperating
scheduler per socket. Locks are keyed by the cleaned socket path, **not engine
identity**: symlinked/aliased socket paths or unrelated schedulers can evade
exclusion. Run only one scheduler per engine. `podman-runit doctor` reports
lock and service state but cannot prove no other scheduler implementation is
active.

## Recovery journal and timeouts

Before dispatching a probe, a private runtime journal records an uncertainty
hold of `2 × (probe timeout + 30s) + interval`. Successful responses clear the
hold; transport or API errors trigger reconciliation and defer another attempt
by at least one request budget. The journal survives a daemon crash, and a
fresh inspect is performed before retry—HTTP success never substitutes for
Podman's cumulative health state. Recovery holds stay conservative even if a
new health-log entry is already visible.

- An omitted timeout defaults to `30s`.
- A zero/unlimited timeout uses a 5-minute client budget plus 30 s overhead (a
  server-side unlimited command may outlive that budget).
- Timeouts over 24 hours are visibly excluded.
- Because the Podman API exposes neither idempotency tokens nor a reliable
  in-flight marker, there is no strict exactly-once or cross-crash no-overlap
  guarantee. Avoid unlimited probe timeouts if that ambiguity matters.

## Shutdown and status

`SIGTERM` stops dispatch and discovery, then drains in-flight requests for 8
seconds by default before cancelling them and retaining the recovery journal
(client cancellation does not terminate server-side probes). Use `sv -w 15 down`
for a clean stop. An immediate kill or restart uses the same recovery holds.

The runtime status file tracks `waiting`, `synchronizing`, `ready`, `degraded`,
`stopping`, and `stopped` states along with PID, a 10-second heartbeat,
cumulative metadata/event/probe request counts, maximum scheduling delay (in
nanoseconds), and individually excluded containers. `podman-runit status` and
runit `check` scripts reject stale or dead-process readiness.
