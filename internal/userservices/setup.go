package userservices

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const Marker = ".podman-runit-owned"

var Names = []string{"podman-api", "podman-healthcheckd"}

type Options struct {
	Dir, Assets, Socket, ConfigFile string
	SchedulerOnly, AdoptAPI, DryRun bool
	Out                             io.Writer
}

func Directory(explicit string) (string, error) {
	if explicit == "" {
		explicit = os.Getenv("SVDIR")
	}
	if explicit == "" {
		explicit = filepath.Join(os.Getenv("HOME"), ".config/service")
	}
	if strings.Contains(explicit, ":") || !filepath.IsAbs(explicit) {
		return "", errors.New("service directory must be one absolute directory, not a search path")
	}
	return filepath.Clean(explicit), nil
}
func Owned(path string) bool {
	b, err := os.ReadFile(filepath.Join(path, Marker))
	return err == nil && string(b) == "podman-runit-v1\n"
}
func control(dir, action string) error {
	if _, err := os.Stat(filepath.Join(dir, "supervise/control")); err != nil {
		return nil
	}
	cmd := exec.Command("sv", "-w", "15", action, dir)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sv %s: %s: %w", action, b, err)
	}
	return nil
}
func Enable(o Options) error {
	names := Names
	if o.SchedulerOnly {
		names = names[1:]
		if o.AdoptAPI {
			return errors.New("adopt-api and scheduler-only cannot be combined")
		}
	}
	// Service symlinks resolve relative to the service directory, so they need
	// absolute targets.
	assets, err := filepath.Abs(o.Assets)
	if err != nil {
		return err
	}
	o.Assets = assets
	// Validate all inputs before touching any service.
	for _, name := range names {
		for _, p := range []string{"run", "check", "log/run"} {
			s, err := os.Stat(filepath.Join(o.Assets, name, p))
			if err != nil {
				return err
			}
			if s.Mode()&0111 == 0 {
				return fmt.Errorf("non-executable asset: %s/%s", name, p)
			}
		}
		dest := filepath.Join(o.Dir, name)
		s, err := os.Lstat(dest)
		if err == nil {
			if !s.IsDir() {
				return fmt.Errorf("refusing non-directory service %s", dest)
			}
			if !Owned(dest) && !(name == "podman-api" && o.AdoptAPI) {
				return fmt.Errorf("unmanaged service %s; use --scheduler-only or explicitly --adopt-api", dest)
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if !o.SchedulerOnly && !o.AdoptAPI && !Owned(filepath.Join(o.Dir, Names[0])) {
		if conn, err := net.DialTimeout("unix", o.Socket, time.Second); err == nil {
			conn.Close()
			return errors.New("socket is already served; use --scheduler-only or explicitly adopt the API service")
		}
		if _, err := os.Lstat(o.Socket); err == nil {
			return errors.New("socket path already exists; inspect its ownership manually (never automatically unlinked)")
		}
	}
	for _, name := range names {
		fmt.Fprintf(o.Out, "enable %s (assets %s)\n", filepath.Join(o.Dir, name), o.Assets)
		if name == "podman-api" && o.AdoptAPI {
			fmt.Fprintln(o.Out, "  stop API; back up run/check; preserve config and log service")
		}
	}
	if o.DryRun {
		return nil
	}
	if o.ConfigFile != "" {
		if err := os.MkdirAll(filepath.Dir(o.ConfigFile), 0700); err != nil {
			return err
		}
		file, err := os.OpenFile(o.ConfigFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			_, err = file.WriteString("# Shared trusted POSIX shell configuration.\n# PODMAN_RUNIT_SOCKET=\"$XDG_RUNTIME_DIR/podman/podman.sock\"\n")
			closeErr := file.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		} else if !os.IsExist(err) {
			return err
		}
	}
	if err := os.MkdirAll(o.Dir, 0700); err != nil {
		return err
	}
	for _, name := range names {
		dest := filepath.Join(o.Dir, name)
		if Owned(dest) {
			if err := os.Remove(filepath.Join(dest, "down")); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := control(dest, "up"); err != nil {
				return err
			}
			continue
		}
		_, err := os.Stat(dest)
		adopting := err == nil
		stage := dest
		if adopting {
			if err := os.WriteFile(filepath.Join(dest, "down"), nil, 0600); err != nil {
				return err
			}
			if err := control(dest, "down"); err != nil {
				return err
			}
			backup, err := os.MkdirTemp(dest, ".before-adoption-")
			if err != nil {
				return err
			}
			for _, p := range []string{"run", "check"} {
				if _, err := os.Lstat(filepath.Join(dest, p)); err == nil {
					if err := os.Rename(filepath.Join(dest, p), filepath.Join(backup, p)); err != nil {
						return err
					}
				}
			}
		} else {
			stage, err = os.MkdirTemp(o.Dir, ".podman-runit-")
			if err != nil {
				return err
			}
		}
		if err := install(stage, name, o.Assets); err != nil {
			return err
		}
		if !adopting {
			if err := os.Rename(stage, dest); err != nil {
				return err
			}
		}
		if err := os.Remove(filepath.Join(dest, "down")); err != nil {
			return err
		}
		if err := control(dest, "up"); err != nil {
			return err
		}
	}
	return nil
}
func install(dest, name, assets string) error {
	if err := os.WriteFile(filepath.Join(dest, "down"), nil, 0600); err != nil {
		return err
	}
	for _, p := range []string{"run", "check"} {
		if err := os.Symlink(filepath.Join(assets, name, p), filepath.Join(dest, p)); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "log")); os.IsNotExist(err) {
		if err := os.Mkdir(filepath.Join(dest, "log"), 0700); err != nil {
			return err
		}
		if err := os.Symlink(filepath.Join(assets, name, "log/run"), filepath.Join(dest, "log/run")); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "conf")); os.IsNotExist(err) {
		if err := os.WriteFile(filepath.Join(dest, "conf"), []byte("# Trusted POSIX shell assignments; shared socket belongs in common.conf.\n"), 0600); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dest, Marker), []byte("podman-runit-v1\n"), 0600)
}
func Disable(o Options) error {
	names := Names
	if o.SchedulerOnly {
		names = names[1:]
	}
	for i := len(names) - 1; i >= 0; i-- {
		dest := filepath.Join(o.Dir, names[i])
		if !Owned(dest) {
			continue
		}
		fmt.Fprintf(o.Out, "disable %s (preserve files and logs)\n", dest)
		if !o.DryRun {
			if err := os.WriteFile(filepath.Join(dest, "down"), nil, 0600); err != nil {
				return err
			}
			if err := control(dest, "down"); err != nil {
				return err
			}
		}
	}
	return nil
}
