package userservices

import (
	"os"
	"os/exec"
	"path/filepath"
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
