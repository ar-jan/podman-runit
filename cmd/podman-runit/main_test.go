package main

import (
	"os"
	"testing"
)

func TestDisableWithoutRuntimeDirectory(t *testing.T) {
	// su and sudo often leave XDG_RUNTIME_DIR unset during uninstall.
	t.Setenv("PODMAN_RUNIT_CONFIG_LOADED", "1")
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("PODMAN_RUNIT_SOCKET", "")
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	os.Args = []string{"podman-runit", "disable", "--service-dir", t.TempDir()}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"podman-runit", "status", "--service-dir", t.TempDir()}
	if err := run(); err == nil {
		t.Fatal("status resolved a socket without a runtime directory")
	}
}
