package config

import (
	"os"
	"os/exec"
	"path/filepath"
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

func TestLockHolders(t *testing.T) {
	dir := t.TempDir()
	lock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	holders, err := LockHolders(filepath.Join(dir, "lock"))
	if err != nil || len(holders) != 1 || holders[0].PID != os.Getpid() || holders[0].Exe != exe {
		t.Fatalf("holders %+v (%v), want this process %s", holders, err, exe)
	}
	lock.Close()
	if holders, _ := LockHolders(filepath.Join(dir, "lock")); len(holders) != 0 {
		t.Fatalf("released lock still held by %+v", holders)
	}
	// An open file without a flock has no holder.
	plain, err := os.OpenFile(filepath.Join(dir, "plain"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if holders, _ := LockHolders(plain.Name()); len(holders) != 0 {
		t.Fatalf("unlocked file reported as held by %+v", holders)
	}
	if _, err := LockHolders(filepath.Join(dir, "absent")); !os.IsNotExist(err) {
		t.Fatal("missing lock file not reported", err)
	}
}
func TestDescends(t *testing.T) {
	self := os.Getpid()
	child := exec.Command("sleep", "10")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	for _, tc := range []struct {
		pid, ancestor int
		want          bool
	}{
		{self, self, true}, {self, os.Getppid(), true}, {child.Process.Pid, self, true}, {self, child.Process.Pid, false}, {self, 0, false},
	} {
		if got := Descends(tc.pid, tc.ancestor); got != tc.want {
			t.Fatalf("Descends(%d, %d) = %t", tc.pid, tc.ancestor, got)
		}
	}
}
