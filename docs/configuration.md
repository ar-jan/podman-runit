# Configuration and Service Management

## Service directory and enablement

Run `podman-runit` as the target user. The service directory is resolved in
order from:

1. `--service-dir`
2. `SVDIR`
3. `~/.config/service`

Colon-separated search paths are rejected.

`podman-runit enable` creates writable user service directories (`podman-api`
and `podman-healthcheckd`) with individual script symlinks into
`/usr/share/podman-runit/services`. New definitions are staged with `down`
before activation. Repeated `enable`/`disable` runs preserve local `conf`,
logs, and supervision data. `--dry-run` reports planned actions without
modifying files or controlling services.

### Existing API services

If an independently managed Podman API already serves the selected socket:

```sh
podman-runit enable --scheduler-only
podman-runit disable --scheduler-only
```

`--scheduler-only` never creates, stops, or modifies the `podman-api` service.

Normal `disable` only affects services carrying the `podman-runit` ownership
marker, and manual services are never silently overwritten. To hand an existing
manual API service over to `podman-runit`:

```sh
podman-runit enable --adopt-api --dry-run
podman-runit enable --adopt-api
```

Adoption stops only that API service, backs up its `run`/`check` scripts into a
unique `.before-adoption-*` directory, preserves its logger and local `conf`,
and links the packaged scripts.

## Configuration files and precedence

Both services resolve the same Unix socket; `DOCKER_HOST` has no effect on the
scheduler. Precedence (lowest to highest) is:

1. Inherited environment
2. Turnstile envdir (`TURNSTILE_ENV_DIR`, e.g. `~/.config/service-env`)
3. Shared `${XDG_CONFIG_HOME:-$HOME/.config}/podman-runit/common.conf`
4. Service-local `conf` inside the service directory
5. Explicit binary flags

Config files are sourced as trusted POSIX shell assignments, must be owned by
the user, and must not be group- or other-writable. Assignments are exported.

```sh
# ~/.config/podman-runit/common.conf
PODMAN_RUNIT_SOCKET="$XDG_RUNTIME_DIR/podman/podman.sock"
```

- Set `PODMAN_RUNIT_SOCKET` in `common.conf` before enabling either service.
  Service-local `conf` files cannot override the shared socket.
- `XDG_RUNTIME_DIR` must be a user-owned private directory (`0700`); there is
  no `/tmp` or rootful fallback.
- Service-local `conf` for `podman-healthcheckd` can set `PODMAN_RUNIT_WORKERS`,
  `PODMAN_RUNIT_RECONCILE`, and `PODMAN_RUNIT_DRAIN`. Run
  `podman-healthcheckd --help` for flag defaults and ranges.

## Socket handling

The `podman-api` launcher holds an endpoint lock across `exec` of
`/usr/bin/podman`. It refuses live or foreign sockets and never deletes a
socket during `enable`/`disable`. On startup after an API crash, it removes a
stale socket only if it is owned by the same user, lives in a private
directory, refuses connections, and keeps an unchanged inode.

## Turnstile, readiness, and lingering

Turnstile loads `~/.config/service-env`; the service launchers import
`TURNSTILE_ENV_DIR` before deriving paths. Other user runit supervisors can
supply the environment directly.

Readiness checks (`check`) are separate from liveness; failed checks do not
restart services. Nothing is added to `turnstile-ready` automatically. If you
want containers and health checks to survive logout, configure turnstile
lingering separately.

## Logging

Each service logs via `svlogd` to
`${XDG_STATE_HOME:-$HOME/.local/state}/svlog/<service>`. Its initial `config`
retains five 1 MiB files; existing log configurations are preserved.

Successful probes and normal failing probes are quiet. Infrastructure errors
are rate-limited, and unsupported container configurations emit one diagnostic
per configuration change.

## Upgrading and uninstalling

- **Upgrading:** Package-owned symlinks pick up updated scripts on the next
  service restart. Restart `podman-api` and `podman-healthcheckd` after
  upgrading binaries (keep both stopped during replacement if the launch
  protocol changes). The recovery journal prevents immediate duplicate probes
  across a scheduler restart.
- **Uninstalling:** Run `podman-runit disable` as each user who enabled the
  services, then remove the package with `xbps-remove`. User service
  directories, local `conf`, adoption backups, and logs are intentionally left
  intact and can be removed manually after review. System services under
  `/etc/sv/podman` are untouched.
