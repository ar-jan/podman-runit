#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."
mkdir -p dist
archive="$PWD/dist/podman-runit-0.1.0.tar.gz"
# Explicit source list avoids build artifacts and self-referential checksums.
# Fixed metadata makes byte-identical archives from identical sources.
tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner \
    --exclude=__pycache__ --transform='s,^,podman-runit-0.1.0/,' \
    -cf - go.mod cmd internal services packaging tests Makefile README.md README.voidlinux.md LICENSE \
    | gzip -n > "$archive"
checksum=$(sha256sum "$archive" | cut -d ' ' -f 1)
sed -e "s|@SOURCE_ARCHIVE@|$archive|g" -e "s|@SHA256@|$checksum|g" packaging/void/template > dist/template
printf '%s  %s\n' "$checksum" "$archive" > dist/SHA256SUMS
printf 'Local archive: %s\nConcrete recipe: %s/dist/template\n' "$archive" "$PWD"
