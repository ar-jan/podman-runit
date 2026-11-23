# podman-runit

A lightweight Go healthcheck scheduler for rootless Podman on Void Linux,
shipped with `runit`/`turnstile` user services.

Ordinary image, CLI, and Docker Compose health checks are discovered
automatically without labels. Podman executes the probes and manages timeouts,
retries, health logs, start periods, and failure actions; `podman-healthcheckd`
only schedules when to invoke them over Podman's Unix API socket.

## Requirements

- Void Linux with rootless `podman` (6.1.4 or a later 6.x), `runit`, and `turnstile`
- An enabled Podman event backend (`events_logger = "file"` or `"journald"` in
  `containers.conf`)
- Optional: Docker Compose (used through `podman compose`)

## Installation

### From source

```sh
make check
make install DESTDIR="$PWD/build/stage"
```

### Local XBPS package

Generate a local source tarball and `xbps-src` template with `make archive`:

```sh
make archive
install -Dm644 dist/template /path/to/void-packages/srcpkgs/podman-runit/template
cd /path/to/void-packages
./xbps-src -Q pkg podman-runit
```

See [docs/packaging.md](docs/packaging.md) for chroot/cache notes and
`xbps-create` packaging.

## Usage

Run `podman-runit` as your regular user to set up and manage the `podman-api`
and `podman-healthcheckd` user services (under `--service-dir`, `SVDIR`, or
`~/.config/service`):

```sh
podman-runit enable --dry-run
podman-runit enable
podman-runit status
podman-runit doctor
sv status podman-api podman-healthcheckd
```

To disable the managed services:

```sh
podman-runit disable
```

If you already run your own `podman-api` service, pass `--scheduler-only` to
`enable`/`disable`, or use `podman-runit enable --adopt-api` to hand an
existing manual API service over to `podman-runit`. See
[docs/configuration.md](docs/configuration.md) for details on service
management, upgrades, and uninstalling.

## Configuration

Both services share the Unix socket configured in
`${XDG_CONFIG_HOME:-~/.config}/podman-runit/common.conf` (default:
`$XDG_RUNTIME_DIR/podman/podman.sock`):

```sh
# ~/.config/podman-runit/common.conf
PODMAN_RUNIT_SOCKET="$XDG_RUNTIME_DIR/podman/podman.sock"
```

Per-service tuning (such as `PODMAN_RUNIT_WORKERS`, `PODMAN_RUNIT_RECONCILE`,
and `PODMAN_RUNIT_DRAIN`) can be set in each service directory's `conf` file.
To exclude a specific container from scheduled health checks, set the label
`io.podman.runit.healthcheck=false`.

See [docs/configuration.md](docs/configuration.md) for configuration
precedence, logging, and turnstile/lingering notes, and
[docs/architecture.md](docs/architecture.md) for compatibility policies,
scheduling semantics, and crash recovery.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
make check
```

To run the end-to-end integration suite against an isolated disposable store
(requires rootless containers, `curl`, `runit`, Podman 6.1.4 or a later 6.x, and Docker
Compose):

```sh
python3 tests/integration/run.py --podman /usr/bin/podman --keep
```

Unit tests simulate scheduler clock advancement without sleeps and use Unix
HTTP test servers. See [tests/integration/REPORT.md](tests/integration/REPORT.md)
for validation notes.
