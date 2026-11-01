# Void Linux installation

Requires stock `podman` 6.1.4, `runit`, and `turnstile`. Docker Compose is optional
for the package; use it through `podman compose` when running Compose workloads.
The companion conflicts with the legacy custom package and never provides
`podman`. It contains no Podman binary, replacement metadata, install hooks,
home-directory edits, global enablement or turnstile configuration changes.
Legacy removal/migration/rollback is a separate manual task.

## Build locally

The project has no published source repository or release. Do not submit the
placeholder source recipe as a release recipe. `make archive` generates a
reproducible local tarball and a concrete template with its actual SHA256:

```sh
make archive
install -Dm644 dist/template /path/to/void-packages/srcpkgs/podman-runit/template
# Run in that checkout, after normal xbps-src bootstrap:
./xbps-src -Q pkg podman-runit
```

`packaging/void/template` is canonical; regenerate and synchronize after source
changes. The generated `file://` URL references this checkout's archive. For a
chroot build, preseed that archive under
`hostdir/sources/podman-runit-0.1.0/` (same filename); xbps-src verifies its checksum
before using the cache. Go uses only the standard library; vendor mode requires
no dependency downloads. Normal XBPS checks run Go unit tests; container tests
are explicitly separate.

An alternative local artifact can be constructed from the staged install:

```sh
make install DESTDIR="$PWD/build/stage"
cd build
xbps-create -A x86_64 -n podman-runit-0.1.0_1 \
  -s 'Rootless Podman health scheduling with runit user services' \
  -l MIT -D 'podman>=6.1.4 runit turnstile' \
  -C 'podman-runit-healthcheck>=0' stage
```

Before a public release, assign a real upstream URL/maintainer and immutable
release archive/checksum. The locally generated archive and recipe are for
review and local builds. Never invent a hosted URL or use an unverifiable hash.

## Enable services

Run the helper as the target user. The service directory is `--service-dir`,
then `SVDIR`, then `~/.config/service`; colon-separated search paths are rejected.
The helper creates writable user service directories with individual script
symlinks into `/usr/share/podman-runit/services`. It stages new definitions with
`down` before activation. Repeated enable/disable preserves local `conf`, logs,
and supervision data. `--dry-run` does not write or control services.

```sh
podman-runit enable --dry-run
podman-runit enable
podman-runit doctor
sv status podman-api podman-healthcheckd
```

If an independently managed API already serves the selected socket:

```sh
podman-runit enable --scheduler-only
podman-runit disable --scheduler-only
```

Scheduler-only mode never creates, stops or changes the API service. Normal
disable affects only services carrying the helper ownership marker. A manual
service is never silently overwritten. To explicitly adopt a manual API service:

```sh
podman-runit enable --adopt-api --dry-run
podman-runit enable --adopt-api
```

Adoption stops only that API, moves its run/check scripts into a unique
`.before-adoption-*` directory, keeps its logger and local configuration, and
links packaged scripts. This is an opt-in helper capability, not an automatic
migration. It does not touch any historical health-check implementation.
Inspect the dry run before opting into adoption.

For a different Unix socket, set `PODMAN_RUNIT_SOCKET` in the shared config before
enabling either service. A functioning existing socket is a reuse/adoption
choice. Setup does not delete it. The API service validates private directory
ownership and exclusively locks its endpoint before starting.

Turnstile loads `~/.config/service-env`; the launchers import `TURNSTILE_ENV_DIR`
before deriving paths. Another user runit supervisor may supply the environment
directly. Readiness checks are separate from liveness; failed checks do not
restart services. Nothing is added to `turnstile-ready`. Opt-in login readiness
should be configured only if waiting for container health infrastructure is
desired. Configure turnstile lingering separately if containers and health checks
must survive logout.

## Upgrade and uninstall

Keep both services stopped during replacement if their launch protocol changes.
Otherwise package-owned symlinks resolve updated scripts on the next service
restart. Restart the API and scheduler after upgrading their binaries; runit
processes do not automatically adopt new executable contents. The recovery
journal protects against immediate repeated executions during scheduler restart.

Before uninstalling, run `podman-runit disable` as each user who enabled it.
Remove the package with the normal package manager. User directories, local
configuration, adoption backups and logs are intentionally preserved; remove
only reviewed user-owned files manually. Reinstallation plus `enable` reuses
those directories. The stock package's `/etc/sv/podman` services are untouched.
