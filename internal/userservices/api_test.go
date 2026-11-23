package userservices

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestSocketPreparation(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	socket := filepath.Join(dir, "api")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	if err := prepareSocket(socket); err == nil {
		t.Fatal("live socket removed")
	}
	l.Close()
	if err := prepareSocket(socket); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatal("stale socket retained")
	}
	os.WriteFile(socket, []byte("unrelated"), 0600)
	if err := prepareSocket(socket); err == nil {
		t.Fatal("regular file removed")
	}
}
func TestSocketDirectoryModes(t *testing.T) {
	// $XDG_RUNTIME_DIR/podman is often 0755 when created by a plain mkdir -p.
	for _, tc := range []struct {
		mode os.FileMode
		ok   bool
	}{{0700, true}, {0755, true}, {0750, true}, {0775, false}, {0757, false}, {0777, false}} {
		dir := t.TempDir()
		socket := filepath.Join(dir, "api")
		l, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		l.SetUnlinkOnClose(false)
		l.Close()
		if err := os.Chmod(dir, tc.mode); err != nil {
			t.Fatal(err)
		}
		err = prepareSocket(socket)
		if (err == nil) != tc.ok {
			t.Fatalf("mode %o: %v", tc.mode, err)
		}
		if _, statErr := os.Lstat(socket); tc.ok != os.IsNotExist(statErr) {
			t.Fatalf("mode %o: stale socket removed=%t", tc.mode, os.IsNotExist(statErr))
		}
	}
}
