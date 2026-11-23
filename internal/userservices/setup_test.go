package userservices

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

func TestDisableOfflineServices(t *testing.T) {
	o := opts(t)
	if err := Enable(o); err != nil {
		t.Fatal(err)
	}
	for _, name := range Names {
		dir := filepath.Join(o.Dir, name, "supervise")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		for _, fifo := range []string{"ok", "control"} {
			if err := syscall.Mkfifo(filepath.Join(dir, fifo), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := Disable(o); err != nil {
		t.Fatal(err)
	}
	for _, name := range Names {
		if _, err := os.Stat(filepath.Join(o.Dir, name, "down")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAdoptionKeepsManualServiceOnFailure(t *testing.T) {
	for _, problem := range []string{"dangling logger", "activation", "activation while down"} {
		t.Run(problem, func(t *testing.T) {
			o := opts(t)
			o.AdoptAPI = true
			dest := filepath.Join(o.Dir, "podman-api")
			if err := os.MkdirAll(filepath.Join(dest, "supervise"), 0700); err != nil {
				t.Fatal(err)
			}
			write := func(path, content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dest, path), []byte(content), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write("run", "#!/bin/sh\nexit 0\n")
			write("check", "original check")
			if problem == "dangling logger" {
				if err := os.Symlink("missing", filepath.Join(dest, "log")); err != nil {
					t.Fatal(err)
				}
			} else {
				// Simulate a live supervisor which rejects only the replacement.
				// Its previous request can differ from the persistent down marker.
				ok := filepath.Join(dest, "supervise/ok")
				if err := syscall.Mkfifo(ok, 0600); err != nil {
					t.Fatal(err)
				}
				reader, err := os.OpenFile(ok, os.O_RDONLY|syscall.O_NONBLOCK, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				status := make([]byte, 20)
				status[17] = 'u'
				if problem == "activation while down" {
					status[17] = 'd'
					write("down", "original down")
				}
				write("supervise/status", string(status))
				write("sv", "#!/bin/sh\necho \"$3\" >> \"$4/actions\"\n[ \"$3\" != up ] || [ ! -f \"$4/.podman-runit-owned\" ]\n")
				t.Setenv("PATH", dest+":"+os.Getenv("PATH"))
			}
			if err := Enable(o); err == nil {
				t.Fatal("expected adoption to fail")
			}
			for name, want := range map[string]string{"run": "#!/bin/sh\nexit 0\n", "check": "original check"} {
				b, err := os.ReadFile(filepath.Join(dest, name))
				if err != nil || string(b) != want {
					t.Fatal(name, string(b), err)
				}
			}
			if Owned(dest) {
				t.Fatal("failed adoption claimed manual service")
			}
			down, err := os.ReadFile(filepath.Join(dest, "down"))
			if problem == "activation while down" {
				if err != nil || string(down) != "original down" {
					t.Fatal("lost original down marker", err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatal("left service disabled", err)
			}
			if problem != "dangling logger" {
				actions, _ := os.ReadFile(filepath.Join(dest, "actions"))
				want := "down up down"
				if problem == "activation" {
					want += " up"
				}
				if strings.Join(strings.Fields(string(actions)), " ") != want {
					t.Fatal("wrong runtime state", string(actions))
				}
				for _, name := range []string{"log", "conf"} {
					if _, err := os.Lstat(filepath.Join(dest, name)); !os.IsNotExist(err) {
						t.Fatal("left staged entry", name)
					}
				}
			}
		})
	}
}
