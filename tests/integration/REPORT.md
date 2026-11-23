# Validation report — 2026-11-01

Implementation and local validation; no live installation, migration or rollback.
The installed legacy package and its active recipe/services were not changed.

## Environment and isolation

- Void Linux x86_64, kernel 6.18.59_1, rootless UID 1000, runc 1.5.2.
- Host compiler: Go 1.27.3; XBPS compiler: Go 1.27.2. Module language baseline: Go 1.24.
- Engine: **unmodified stock podman-6.1.4_1.x86_64.xbps**, extracted from the
  cached package into `build/stock`, never installed over the host engine.
- Stock package SHA256:
  `6195db2714b042e55ac59b97891f2e161e4805aea9568dbbca0b00781afbedc0`.
- Separate temporary vfs graphroot, runroot, network config, containers.conf,
  runtime directory and Unix API socket; both CLI and API target this store.
- BusyBox `docker.io/library/busybox:1.37.0`; real Docker Compose invoked through
  `podman compose`. The suite explicitly unsets ambient remote-engine selection.
- API and scheduler run under two disposable runsv supervisors. No changes to
  the user's turnstile services or live container store.

## Automated results

Passed `go test ./...`, `go test -race ./...`, `go vet ./...`, POSIX shell syntax
checks and ShellCheck. Tests cover integer duration decoding, native version
selection, real Unix HTTP requests, malformed responses, 404/409/500, dropped
health responses without replay, redirects, event schemas, subscription before
snapshot, initial API absence, event overlap/replay/overflow/reconnect, partial
snapshot rejection, bounded four-worker execution, metadata progress during
blocked probes, bounded shutdown and recovery journal retention.

Deterministic time advancement covers prompt first execution, grace periods,
subsecond intervals, completion-based timing, no overlap/catch-up, health history
from older starts, uncertain recovery, pause/unpause, removal, configuration
changes and 10,000 generation replacements. Kernel-lock tests cover exclusion,
release on normal close and process exit without cleanup. Setup tests cover
dry run, idempotency, ownership, writable directories, config preservation,
scheduler-only isolation, manual API adoption backups, logger preservation,
turnstile/shared/local environment precedence, and refusal to change the shared
socket locally. Socket preparation refuses a live socket or ordinary file and
recovers a verified stale same-user socket.

Stock engine end-to-end suite **passed**, including:

- Existing and newly created containers; ordinary defaults and omitted disabled
  intervals; CMD and CMD-SHELL; successful, failing and timed-out probes.
- Engine retries leading to unhealthy, successful probes during a 60s start
  period, and configured failure action `stop`.
- Interval updates; independent scheduler and API restarts, verified by **new
  health-log completion timestamps**; three further scheduler restarts.
- Startup probes visibly excluded; disabled checks receive no probes.
- Actual Compose `condition: service_healthy`: dependent service started in
  **1.238s**, with the checked service's start period set to 60s.
- Compose `start_interval: 250ms` was discarded by stock compatibility creation;
  native inspect retained interval 1s and start period 60s, with no start interval.

Sanitized native inspect fixtures are committed under
`internal/podmanapi/testdata/`; they contain no environment or probe output.
Full local run evidence is `build/integration-evidence.json` (ignored build
artifact), and each `--keep` run prints its temporary directory.

## Resource observations

Each scenario samples `/proc` for three wall-clock seconds; CPU uses user+system
ticks at the host clock rate. Recursive process enumeration starts at both
runsv supervisors, including the API's rootless re-exec child where present.
These totals describe the infrastructure group, not container workloads or a
peak concurrent probe measurement. No logger processes were enabled in this
measurement. The two runsv processes are included; they use about 97KiB PSS each.
Background compiler activity was present during part of this run, so these are
short observations, not controlled idle-machine benchmarks.

| Scenario | Seconds | Processes | Total RSS KiB | Total PSS KiB | CPU seconds | Scheduler RSS / PSS KiB | Scheduler FDs |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| no containers | 3.01 | 5 | 113544 | 66955 | 0.01 | 7504 / 7496 | 8 |
| one container without checks | 3.01 | 5 | 115548 | 68895 | 0.02 | 7548 / 7540 | 8 |
| three checks with 30s intervals | 3.01 | 5 | 117116 | 70335 | 0.01 | 7744 / 7736 | 8 |
| seven short interval checks including slow failing probes | 3.03 | 5 | 123876 | 77089 | 0.15 | 9320 / 9312 | 11 |
| after repeated scheduler restarts | 3.00 | 4 | 73820 | 64039 | 0.10 | 8480 / 8472 | 9 |

The scheduler column is the incremental daemon cost when an API already exists;
add its runsv supervisor for the supervised increment. RSS sums double-count
shared pages; PSS apportions them. The API initially had a rootless parent and
re-exec child; after API restart it used one process, explaining the change in
group size and memory. Scheduler restart samples show no accumulating scheduler
processes or file descriptors, but are not a long-duration leak proof.

The final scheduler generation reported 19 metadata
requests, 32 health requests and 1
event stream, with maximum observed scheduling delay
98.71ms at its last status snapshot. Counters
reset per scheduler process and status can lag up to one heartbeat. These values
are not whole-suite totals. A worker-pool test independently proves at most four
concurrent scheduler-issued health requests, including when metadata refreshes.

Structural checks: no Podman subprocess in the scheduler; no per-container
process/service; bounded event buffer, pending-ID set, worker pool and heap;
no work catch-up; reconnect backoff; 60s full reconciliation and 10s status
heartbeat. Podman's file event reader retains its own 250ms polling baseline.

## Packaging and remaining boundaries

The full `xbps-src pkg podman-runit` build completed successfully, including
Go build-helper compilation, installation, stripping, runtime dependency
resolution and package lint hooks. The resulting source-built package is under
`void-packages/hostdir/binpkgs/extra/` and copied to this project's `dist/` for
handoff. The recipe was installed at
`/home/arjan/void-packages/srcpkgs/podman-runit/template`. No host package was
installed. The build chroot built its pinned Go 1.27.2 prerequisite locally.
XBPS adds ordinary command-provides metadata and detected glibc dependencies;
it does not provide a virtual Podman package.

`make install DESTDIR=...` stages two executables, package-owned service scripts,
MIT license and docs with executable modes. The project owns a local-source
recipe; `make archive` emits deterministic archive metadata, an actual checksum,
and a concrete template. No published repository URL is claimed. A release still
needs an upstream location and maintainer metadata. The source package conflicts
with the old custom build, does not provide Podman and has no replacement or
migration hooks.

Native API 6.1.4 or a later 6.x is accepted; only 6.1.4 was validated live. Podman startup probes are intentionally
unsupported. Compose start_interval is an observed engine limitation. Crash or
connection-loss exactly-once execution is impossible with this API; the durable
hold and refresh policy prevents immediate retries but cannot prove server-side
completion, especially for unlimited probes. Rootful/TCP, non-file event backend
live validation, long-duration endurance, actual login/logout/lingering, and
peak aggregate probe memory were not validated. The live manual API service was
not adopted; adoption is tested with isolated fixtures.

No legacy cleanup, rollback archive, package replacement transaction or retirement
was implemented or executed. Those remain a separate manual operation as requested.
