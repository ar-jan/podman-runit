package userservices

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

const Marker = ".podman-runit-owned"
const ownership = "podman-runit-v2\n"
const legacyOwnership = "podman-runit-v1\n"

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
	return err == nil && (string(b) == ownership || string(b) == legacyOwnership)
}

// Keep adopted loggers independent. Older installations marked only the parent
// and installed log/run as a symlink into the same asset directory as run.
func serviceParts(dest string) []string {
	parts := []string{dest}
	logDir := filepath.Join(dest, "log")
	if info, err := os.Lstat(logDir); err != nil || !info.IsDir() {
		return parts
	}
	if Owned(logDir) {
		return append(parts, logDir)
	}
	marker, _ := os.ReadFile(filepath.Join(dest, Marker))
	if string(marker) == legacyOwnership {
		main, err := os.Readlink(filepath.Join(dest, "run"))
		logger, logErr := os.Readlink(filepath.Join(logDir, "run"))
		if err == nil && logErr == nil && logger == filepath.Join(filepath.Dir(main), "log/run") {
			return append(parts, logDir)
		}
	}
	return parts
}

// runsv leaves its FIFOs behind on exit. Like sv, probe the ok FIFO for a
// reader to distinguish a live supervisor from an offline service directory.
func supervised(dir string) (bool, error) {
	f, err := os.OpenFile(filepath.Join(dir, "supervise/ok"), os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) || errors.Is(err, syscall.ENXIO) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, f.Close()
}
func control(dir, action string) error {
	if live, err := supervised(dir); err != nil || !live {
		return err
	}
	cmd := exec.Command("sv", "-w", "15", action, dir)
	if b, err := cmd.CombinedOutput(); err != nil {
		if live, checkErr := supervised(dir); checkErr == nil && !live {
			return nil
		}
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
	lock, err := lockServiceDir(o.Dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	for _, name := range names {
		if err := enableService(o, name); err != nil {
			return err
		}
	}
	return nil
}

// Serialize helper changes across the ownership check and adoption rollback.
func lockServiceDir(dir string) (*os.File, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func enableService(o Options, name string) error {
	dest := filepath.Join(o.Dir, name)
	info, err := os.Lstat(dest)
	switch {
	case err == nil && !info.IsDir():
		return fmt.Errorf("refusing non-directory service %s", dest)
	case err == nil && Owned(dest):
		return activate(dest)
	case err == nil && !(o.AdoptAPI && name == "podman-api"):
		return fmt.Errorf("refusing to adopt unmanaged service %s", dest)
	case err != nil && !os.IsNotExist(err):
		return err
	}

	stage, err := os.MkdirTemp(o.Dir, ".podman-runit-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := install(stage, name, o.Assets); err != nil {
		return err
	}
	if info != nil {
		return adoptService(dest, stage)
	}
	if err := os.Rename(stage, dest); err != nil {
		return err
	}
	return activate(dest)
}

func activate(dest string) error {
	for _, dir := range slices.Backward(serviceParts(dest)) {
		if err := os.Remove(filepath.Join(dir, "down")); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := control(dir, "up"); err != nil {
			return err
		}
	}
	return nil
}

func install(dest, name, assets string) error {
	if err := os.WriteFile(filepath.Join(dest, "down"), nil, 0600); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(dest, "log"), 0700); err != nil {
		return err
	}
	for _, p := range []string{"run", "check", "log/run"} {
		if err := os.Symlink(filepath.Join(assets, name, p), filepath.Join(dest, p)); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dest, "conf"), []byte("# Trusted POSIX shell assignments; shared socket belongs in common.conf.\n"), 0600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dest, "log", Marker), []byte(ownership), 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dest, Marker), []byte(ownership), 0600)
}
func Disable(o Options) error {
	if !o.DryRun {
		lock, err := lockServiceDir(o.Dir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		defer lock.Close()
	}
	names := Names
	if o.SchedulerOnly {
		names = names[1:]
	}
	var errs []error
	for i := len(names) - 1; i >= 0; i-- {
		dest := filepath.Join(o.Dir, names[i])
		if !Owned(dest) {
			continue
		}
		fmt.Fprintf(o.Out, "disable %s (preserve files and logs)\n", dest)
		if !o.DryRun {
			parts := serviceParts(dest)
			for _, dir := range parts {
				if err := os.WriteFile(filepath.Join(dir, "down"), nil, 0600); err != nil {
					errs = append(errs, err)
				}
			}
			// Drain the service before stopping its log reader.
			for _, dir := range parts {
				if err := control(dir, "down"); err != nil {
					errs = append(errs, err)
					break
				}
			}
		}
	}
	return errors.Join(errs...)
}
