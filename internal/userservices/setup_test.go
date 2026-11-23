package userservices

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func opts(t *testing.T) Options {
	t.Helper()
	assets, err := filepath.Abs("../../services")
	if err != nil {
		t.Fatal(err)
	}
	return Options{ConfigFile: filepath.Join(t.TempDir(), "common.conf"), Dir: filepath.Join(t.TempDir(), "sv"), Assets: assets, Socket: filepath.Join(t.TempDir(), "absent"), Out: io.Discard}
}
func TestSetupLifecycle(t *testing.T) {
	o := opts(t)
	o.DryRun = true
	if err := Enable(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(o.Dir); !os.IsNotExist(err) {
		t.Fatal("dry run writes")
	}
	o.DryRun = false
	for i := 0; i < 2; i++ {
		if err := Enable(o); err != nil {
			t.Fatal(err)
		}
	}
	dest := filepath.Join(o.Dir, Names[0])
	if s, err := os.Lstat(dest); err != nil || !s.IsDir() {
		t.Fatal("service directory must be writable")
	}
	conf := filepath.Join(dest, "conf")
	os.WriteFile(conf, []byte("# preserve"), 0600)
	if err := Disable(o); err != nil {
		t.Fatal(err)
	}
	if err := Disable(o); err != nil {
		t.Fatal(err)
	}
	if err := Enable(o); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(conf)
	if string(b) != "# preserve" {
		t.Fatal("configuration overwritten")
	}
}
func TestManualAPIAndAdoption(t *testing.T) {
	o := opts(t)
	api := filepath.Join(o.Dir, Names[0])
	os.MkdirAll(filepath.Join(api, "log"), 0700)
	os.WriteFile(filepath.Join(api, "run"), []byte("manual"), 0700)
	os.WriteFile(filepath.Join(api, "log/run"), []byte("custom log"), 0700)
	if err := Enable(o); err == nil {
		t.Fatal("overwrote manual API")
	}
	if _, err := os.Stat(filepath.Join(o.Dir, Names[1])); !os.IsNotExist(err) {
		t.Fatal("partial preflight mutation")
	}
	o.SchedulerOnly = true
	if err := Enable(o); err != nil {
		t.Fatal(err)
	}
	if err := Disable(o); err != nil {
		t.Fatal(err)
	}
	if Owned(api) {
		t.Fatal("scheduler-only adopted API")
	}
	o.SchedulerOnly = false
	o.AdoptAPI = true
	if err := Enable(o); err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(api, ".before-adoption-*/run"))
	if len(paths) != 1 {
		t.Fatal("missing backup")
	}
	b, _ := os.ReadFile(filepath.Join(api, "log/run"))
	if string(b) != "custom log" {
		t.Fatal("logger overwritten")
	}
}
func TestDirectory(t *testing.T) {
	if _, err := Directory("/a:/b"); err == nil {
		t.Fatal("search path accepted")
	}
	if _, err := Directory("relative"); err == nil {
		t.Fatal("relative directory accepted")
	}
}
func TestRelativeAssets(t *testing.T) {
	o := opts(t)
	o.Assets = "../../services" // relative to this package directory
	if err := Enable(o); err != nil {
		t.Fatal(err)
	}
	for _, name := range Names {
		for _, p := range []string{"run", "check", "log/run"} {
			if _, err := os.Stat(filepath.Join(o.Dir, name, p)); err != nil {
				t.Fatalf("dangling %s/%s: %v", name, p, err)
			}
		}
	}
}
