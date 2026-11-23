package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"podman-runit/internal/config"
	"podman-runit/internal/userservices"
	"strconv"
	"strings"
	"syscall"
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

// engine serves a rootless Podman 6.1.4 whose listing is list; other
// container requests go to inspect.
func engine(t *testing.T, list string, inspect http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pr-doctor-")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "api")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/version":
			w.Header().Set("Libpod-API-Version", "6.1.4")
			fmt.Fprint(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/info"):
			fmt.Fprint(w, `{"host":{"eventLogger":"file","security":{"rootless":true}}}`)
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			fmt.Fprint(w, list)
		default:
			inspect(w, r)
		}
	})}
	go server.Serve(l)
	t.Cleanup(func() { server.Close(); os.RemoveAll(dir) })
	return socket
}

// doctor runs the doctor command and returns its standard output.
func doctor(t *testing.T, socket, serviceDir string) (string, error) {
	t.Helper()
	t.Setenv("PODMAN_RUNIT_CONFIG_LOADED", "1")
	args, stdout := os.Args, os.Stdout
	defer func() { os.Args, os.Stdout = args, stdout }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	captured := make(chan string)
	go func() { b, _ := io.ReadAll(r); captured <- string(b) }()
	os.Args = []string{"podman-runit", "doctor", "--socket", socket, "--service-dir", serviceDir}
	err = run()
	w.Close()
	return <-captured, err
}

func TestDoctorReportsEveryUninspectableContainer(t *testing.T) {
	bad, gone, plain := strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)
	socket := engine(t, fmt.Sprintf(`[{"Id":%q},{"Id":%q},{"Id":%q}]`, bad, gone, plain), func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, bad):
			w.WriteHeader(500)
			fmt.Fprint(w, `{"message":"storage for container missing"}`)
		case strings.Contains(r.URL.Path, gone):
			w.WriteHeader(404)
		default:
			fmt.Fprintf(w, `{"Id":%q}`, plain)
		}
	})
	t.Setenv("XDG_RUNTIME_DIR", "")
	out, err := doctor(t, socket, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "1 of 3 containers") {
		t.Fatalf("doctor result: %v", err)
	}
	if !strings.Contains(out, bad+": inspect failed: Podman HTTP 500: storage for container missing") {
		t.Fatalf("broken container not reported:\n%s", out)
	}
	if !strings.Contains(out, plain+": not running") || strings.Contains(out, gone) {
		t.Fatalf("doctor stopped early or reported a removed container:\n%s", out)
	}
}

func TestDoctorFlagsUnsupervisedAPILockHolder(t *testing.T) {
	socket := engine(t, `[]`, nil)
	xdg := t.TempDir()
	os.Chmod(xdg, 0700)
	t.Setenv("XDG_RUNTIME_DIR", xdg)
	runtime, err := config.Runtime(socket)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(runtime, userservices.APILock), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	services := t.TempDir()
	supervise := filepath.Join(services, userservices.Names[0], "supervise")
	os.MkdirAll(supervise, 0700)
	setSupervised := func(pid int) {
		os.WriteFile(filepath.Join(supervise, "pid"), []byte(strconv.Itoa(pid)+"\n"), 0600)
	}
	held := fmt.Sprintf("API endpoint lock: held by pid %d (", os.Getpid())

	// This test process descends from its parent, as a forked API descends
	// from the process runsv supervises.
	setSupervised(os.Getppid())
	out, err := doctor(t, socket, services)
	if err != nil || !strings.Contains(out, held) || !strings.Contains(out, "), supervised by ") {
		t.Fatalf("supervised holder: %v\n%s", err, out)
	}

	// An unrelated supervised process: the holder outlived its supervisor.
	other := exec.Command("sleep", "10")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { other.Process.Kill(); other.Wait() }()
	setSupervised(other.Process.Pid)
	out, err = doctor(t, socket, services)
	if err == nil || !strings.Contains(err.Error(), "1 unsupervised API process(es)") {
		t.Fatalf("doctor result: %v", err)
	}
	if !strings.Contains(out, held) || !strings.Contains(out, fmt.Sprintf("NOT supervised by %s; if no other service manages this socket, stop it with kill %d", filepath.Join(services, userservices.Names[0]), os.Getpid())) {
		t.Fatalf("unsupervised holder not flagged:\n%s", out)
	}
}

func TestDoctorNeverTakesSchedulerLock(t *testing.T) {
	socket := engine(t, `[]`, nil)
	xdg := t.TempDir()
	os.Chmod(xdg, 0700)
	t.Setenv("XDG_RUNTIME_DIR", xdg)
	runtime, err := config.Runtime(socket)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := config.Lock(runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	// A doctor that never opens the lock file cannot take it, even briefly,
	// so a scheduler starting at that moment never finds it busy.
	watch, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(watch)
	if _, err := syscall.InotifyAddWatch(watch, filepath.Join(runtime, config.SchedulerLock), syscall.IN_OPEN); err != nil {
		t.Fatal(err)
	}
	opened := func() bool {
		buf := make([]byte, 4096)
		n, err := syscall.Read(watch, buf)
		return err == nil && n > 0
	}
	out, err := doctor(t, socket, t.TempDir())
	if err != nil || !strings.Contains(out, fmt.Sprintf("socket scheduler lock: held by pid %d (", os.Getpid())) {
		t.Fatalf("held lock: %v\n%s", err, out)
	}
	if opened() {
		t.Fatal("doctor opened the held scheduler lock")
	}
	lock.Close()
	out, err = doctor(t, socket, t.TempDir())
	if err != nil || !strings.Contains(out, "socket scheduler lock: free") {
		t.Fatalf("free lock: %v\n%s", err, out)
	}
	if opened() {
		t.Fatal("doctor opened the free scheduler lock")
	}
}
