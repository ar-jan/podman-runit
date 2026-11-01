package config

import (
	"os"
	"os/exec"
	"testing"
)

func TestLockAndSocket(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("DOCKER_HOST", "tcp://bad:123")
	t.Setenv("PODMAN_RUNIT_SOCKET", "")
	socket, err := Socket("")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := Runtime(socket)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Lock(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Lock(runtime); err == nil {
		second.Close()
		t.Fatal("duplicate lock acquired")
	}
	first.Close()
	second, err := Lock(runtime)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
	if _, err := Socket("tcp://localhost:1"); err == nil {
		t.Fatal("network socket accepted")
	}
	os.Chmod(dir, 0755)
	if _, err := Runtime(socket); err == nil {
		t.Fatal("public runtime accepted")
	}
}

func TestLockReleasedOnProcessExit(t *testing.T) {
	if dir := os.Getenv("PODMAN_RUNIT_TEST_LOCK_CHILD"); dir != "" {
		f, err := Lock(dir)
		if err != nil {
			os.Exit(2)
		}
		_ = f
		os.Exit(0)
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockReleasedOnProcessExit$")
	cmd.Env = append(os.Environ(), "PODMAN_RUNIT_TEST_LOCK_CHILD="+dir)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	f, err := Lock(dir)
	if err != nil {
		t.Fatal("crashed process retained lock", err)
	}
	f.Close()
}
