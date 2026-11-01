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
