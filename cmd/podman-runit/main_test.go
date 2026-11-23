package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

func TestDoctorReportsEveryUninspectableContainer(t *testing.T) {
	bad, gone, plain := strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)
	dir, err := os.MkdirTemp("", "pr-doctor-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
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
			fmt.Fprintf(w, `[{"Id":%q},{"Id":%q},{"Id":%q}]`, bad, gone, plain)
		case strings.Contains(r.URL.Path, bad):
			w.WriteHeader(500)
			fmt.Fprint(w, `{"message":"storage for container missing"}`)
		case strings.Contains(r.URL.Path, gone):
			w.WriteHeader(404)
		default:
			fmt.Fprintf(w, `{"Id":%q}`, plain)
		}
	})}
	go server.Serve(l)
	t.Cleanup(func() { server.Close() })

	t.Setenv("PODMAN_RUNIT_CONFIG_LOADED", "1")
	t.Setenv("XDG_RUNTIME_DIR", "")
	args, stdout := os.Args, os.Stdout
	t.Cleanup(func() { os.Args, os.Stdout = args, stdout })
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	captured := make(chan string)
	go func() { b, _ := io.ReadAll(r); captured <- string(b) }()
	os.Args = []string{"podman-runit", "doctor", "--socket", socket, "--service-dir", t.TempDir()}
	err = run()
	w.Close()
	out := <-captured
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
