# Void Linux Packaging

`podman-runit` has no external Go dependencies (standard library only, built
with `-mod=vendor` / `go_mod_mode=vendor`). Unit tests run during normal XBPS
checks; container integration tests are kept separate.

## Building with `xbps-src`

`packaging/void/template` is the canonical template. Run `make archive` to
create a reproducible local source tarball and a concrete template populated
with its path and SHA256 checksum:

```sh
make archive
install -Dm644 dist/template /path/to/void-packages/srcpkgs/podman-runit/template
cd /path/to/void-packages
./xbps-src -Q pkg podman-runit
```

For a chroot build, preseed the generated archive under
`hostdir/sources/podman-runit-0.1.0/` (same filename); `xbps-src` verifies the
checksum before using the cache.

## Building directly with `xbps-create`

Alternatively, package a staged install directly:

```sh
make install DESTDIR="$PWD/build/stage"
cd build
xbps-create -A x86_64 -n podman-runit-0.1.0_1 \
  -s 'Rootless Podman health scheduling with runit user services' \
  -l MIT -D 'podman>=6.1.0 runit turnstile' stage
```

## Notes for public releases

Before publishing a release template upstream, replace the local `homepage`,
`distfiles`, `checksum`, and `maintainer` fields in `packaging/void/template`
with real upstream values and an immutable release archive checksum.
