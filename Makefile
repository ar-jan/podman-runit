PREFIX ?= /usr
DESTDIR ?=
GO ?= go

.PHONY: all test check install install-assets clean archive
all:
	mkdir -p build
	CGO_ENABLED=0 $(GO) build -trimpath -o build/podman-healthcheckd ./cmd/podman-healthcheckd
	CGO_ENABLED=0 $(GO) build -trimpath -o build/podman-runit ./cmd/podman-runit

test:
	$(GO) test ./...
check: test
	$(GO) test -race ./...
	$(GO) vet ./...
	find services -type f -exec sh -n {} \;
	shellcheck services/launch services/*/run services/*/check services/*/log/run

install: all install-assets
	install -Dm755 build/podman-healthcheckd $(DESTDIR)$(PREFIX)/bin/podman-healthcheckd
	install -Dm755 build/podman-runit $(DESTDIR)$(PREFIX)/bin/podman-runit

install-assets:
	install -d $(DESTDIR)$(PREFIX)/share/podman-runit/services
	cp -R services/. $(DESTDIR)$(PREFIX)/share/podman-runit/services/
	install -Dm644 LICENSE $(DESTDIR)$(PREFIX)/share/licenses/podman-runit/LICENSE
	install -Dm644 README.md $(DESTDIR)$(PREFIX)/share/doc/podman-runit/README.md
	install -Dm644 README.voidlinux.md $(DESTDIR)$(PREFIX)/share/doc/podman-runit/README.voidlinux.md
	install -Dm644 tests/integration/REPORT.md $(DESTDIR)$(PREFIX)/share/doc/podman-runit/tests/integration/REPORT.md

archive:
	./packaging/void/local-source.sh
clean:
	rm -rf build dist
