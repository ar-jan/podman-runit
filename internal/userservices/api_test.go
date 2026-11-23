package userservices

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
func TestBusyAPILockNamesHolder(t *testing.T) {
	dir := t.TempDir()
	fd, err := lockAPI(dir, "/run/test/api.sock")
	if err != nil {
		t.Fatal(err)
	}
	_, err = lockAPI(dir, "/run/test/api.sock")
	pid := os.Getpid()
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("by pid %d (", pid)) || !strings.Contains(err.Error(), fmt.Sprintf("kill %d", pid)) {
		t.Fatalf("busy lock error does not name its holder: %v", err)
	}
	syscall.Close(fd)
	if fd, err = lockAPI(dir, "/run/test/api.sock"); err != nil {
		t.Fatal("released lock not reacquired:", err)
	}
	syscall.Close(fd)
}
func TestSupervisedPID(t *testing.T) {
	service := t.TempDir()
	if pid := SupervisedPID(service); pid != 0 {
		t.Fatal("missing supervise directory gave", pid)
	}
	os.Mkdir(filepath.Join(service, "supervise"), 0700)
	os.WriteFile(filepath.Join(service, "supervise/pid"), []byte("1234\n"), 0600)
	if pid := SupervisedPID(service); pid != 1234 {
		t.Fatal("supervised pid", pid)
	}
}
