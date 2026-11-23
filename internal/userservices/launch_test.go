package userservices

import (
	"os"
	"os/exec"
	"path/filepath"
	"podman-runit/internal/config"
	"strings"
	"testing"
)

func TestLauncherEnvironmentPrecedence(t *testing.T) {
	if _, err := exec.LookPath("chpst"); err != nil {
		t.Skip("runit needed for envdir launcher test")
	}
	dir := t.TempDir()
	runtime := filepath.Join(dir, "runtime")
	os.Mkdir(runtime, 0700)
	envdir := filepath.Join(dir, "env")
	os.Mkdir(envdir, 0700)
	os.WriteFile(filepath.Join(envdir, "XDG_RUNTIME_DIR"), []byte(runtime), 0600)
	home := filepath.Join(dir, "home")
	shared := filepath.Join(home, ".config/podman-runit")
	os.MkdirAll(shared, 0700)
	os.WriteFile(filepath.Join(shared, "common.conf"), []byte("PODMAN_RUNIT_SOCKET=\"$XDG_RUNTIME_DIR/custom.sock\"\nLOCAL_TEST=shared\n"), 0600)
	service := filepath.Join(dir, "service")
	os.Mkdir(service, 0700)
	os.WriteFile(filepath.Join(service, "conf"), []byte("LOCAL_TEST=service\nPODMAN_RUNIT_WORKERS=2\n"), 0600)
	fake := filepath.Join(dir, "daemon")
	os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$PODMAN_RUNIT_SOCKET\" \"$LOCAL_TEST\" \"$@\"\n"), 0700)
	script, err := os.ReadFile("../../services/launch")
	if err != nil {
		t.Fatal(err)
	}
	launch := filepath.Join(dir, "launch")
	os.WriteFile(launch, []byte(strings.ReplaceAll(string(script), "/usr/bin/podman-healthcheckd", fake)), 0700)
	cmd := exec.Command(launch, "podman-healthcheckd", "--workers", "3")
	cmd.Dir = service
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "XDG_RUNTIME_DIR=/wrong", "TURNSTILE_ENV_DIR=" + envdir}
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(b))
	}
	want := runtime + "/custom.sock\nservice\n--workers\n2\n--reconcile\n60s\n--drain\n8s\n--workers\n3\n"
	if string(b) != want {
		t.Fatalf("got %q want %q", b, want)
	}
	os.WriteFile(filepath.Join(service, "conf"), []byte("PODMAN_RUNIT_SOCKET=/wrong.sock\n"), 0600)
	cmd = exec.Command(launch, "podman-healthcheckd")
	cmd.Dir = service
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "XDG_RUNTIME_DIR=" + runtime}
	if err := cmd.Run(); err == nil {
		t.Fatal("service socket override accepted")
	}
}
func TestLauncherSocketForms(t *testing.T) {
	// The launcher must accept exactly the socket forms config.Socket accepts.
	dir := t.TempDir()
	runtime := filepath.Join(dir, "runtime")
	os.Mkdir(runtime, 0700)
	home := filepath.Join(dir, "home")
	shared := filepath.Join(home, ".config/podman-runit")
	os.MkdirAll(shared, 0700)
	service := filepath.Join(dir, "service")
	os.Mkdir(service, 0700)
	fake := filepath.Join(dir, "daemon")
	os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$PODMAN_RUNIT_SOCKET\"\n"), 0700)
	script, err := os.ReadFile("../../services/launch")
	if err != nil {
		t.Fatal(err)
	}
	launch := filepath.Join(dir, "launch")
	os.WriteFile(launch, []byte(strings.ReplaceAll(string(script), "/usr/bin/podman-healthcheckd", fake)), 0700)
	for _, tc := range []struct{ socket, want string }{
		{runtime + "/podman.sock", runtime + "/podman.sock"},
		{"unix://" + runtime + "/podman.sock", runtime + "/podman.sock"},
		{"unix://relative.sock", ""},
		{"relative.sock", ""},
		{"tcp://localhost:1", ""},
		{"/odd://path.sock", ""},
	} {
		os.WriteFile(filepath.Join(shared, "common.conf"), []byte("PODMAN_RUNIT_SOCKET='"+tc.socket+"'\n"), 0600)
		cmd := exec.Command(launch, "podman-healthcheckd")
		cmd.Dir = service
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "XDG_RUNTIME_DIR=" + runtime}
		b, err := cmd.CombinedOutput()
		if tc.want == "" {
			if err == nil {
				t.Fatalf("%q accepted: %s", tc.socket, b)
			}
			continue
		}
		if err != nil || string(b) != tc.want+"\n" {
			t.Fatalf("%q: got %q (%v), want %q", tc.socket, b, err, tc.want)
		}
		helper, err := config.Socket(tc.socket)
		if err != nil || helper != tc.want {
			t.Fatalf("%q: helper resolved %q (%v)", tc.socket, helper, err)
		}
	}
}
func TestLauncherStreams(t *testing.T) {
	dir := t.TempDir()
	runtime := filepath.Join(dir, "runtime")
	os.Mkdir(runtime, 0700)
	service := filepath.Join(dir, "service")
	os.Mkdir(service, 0700)
	fake := filepath.Join(dir, "fake")
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"fake $*\" >&2\n"), 0700)
	script, err := os.ReadFile("../../services/launch")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(script), "/usr/bin/podman-runit", fake)
	launch := filepath.Join(dir, "launch")
	os.WriteFile(launch, []byte(strings.ReplaceAll(s, "/usr/bin/podman-healthcheckd", fake)), 0700)
	run := func(closeStdout bool, xdg string, args ...string) (string, string, error) {
		// runit's sv runs ./check with stdout closed.
		wrapper := `exec "$0" "$@"`
		if closeStdout {
			wrapper = `exec 1>&-; exec "$0" "$@"`
		}
		cmd := exec.Command("sh", append([]string{"-c", wrapper, launch}, args...)...)
		cmd.Dir = service
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "XDG_RUNTIME_DIR=" + xdg}
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	for _, mode := range []string{"check-api", "check-scheduler"} {
		if _, stderr, err := run(true, runtime, mode); err != nil || stderr != "fake "+mode+"\n" {
			t.Fatalf("%s with stdout closed: %v, stderr %q", mode, err, stderr)
		}
	}
	os.Chmod(runtime, 0755)
	if _, stderr, err := run(true, runtime, "check-api"); err == nil || !strings.Contains(stderr, "runtime directory must be private") {
		t.Fatalf("failing check did not explain itself: %v, stderr %q", err, stderr)
	}
	os.Chmod(runtime, 0700)
	// Run scripts keep stderr merged into stdout, the log pipe.
	if stdout, stderr, err := run(false, runtime, "podman-healthcheckd"); err != nil || !strings.HasPrefix(stdout, "fake --workers") || stderr != "" {
		t.Fatalf("run mode: %v, stdout %q, stderr %q", err, stdout, stderr)
	}
}
