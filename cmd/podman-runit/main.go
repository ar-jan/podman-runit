package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"podman-runit/internal/config"
	"podman-runit/internal/podmanapi"
	"podman-runit/internal/scheduler"
	"podman-runit/internal/userservices"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if err := config.ImportCommon(); err != nil {
		return err
	}
	if len(os.Args) < 2 {
		return errors.New("usage: podman-runit enable|disable|status|doctor|check-api|check-scheduler [options]")
	}
	command := os.Args[1]
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	dirFlag := f.String("service-dir", "", "one user runit service directory")
	assets := f.String("assets", "/usr/share/podman-runit/services", "packaged service assets")
	socketFlag := f.String("socket", "", "absolute Unix socket")
	only := f.Bool("scheduler-only", false, "reuse independently managed API")
	adopt := f.Bool("adopt-api", false, "explicitly adopt manual API, backing up run/check and retaining logger")
	dry := f.Bool("dry-run", false, "describe setup without changes")
	if err := f.Parse(os.Args[2:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	dir, err := userservices.Directory(*dirFlag)
	if err != nil {
		return err
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(os.Getenv("HOME"), ".config")
	}
	o := userservices.Options{ConfigFile: filepath.Join(configHome, "podman-runit/common.conf"), Dir: dir, Assets: *assets, SchedulerOnly: *only, AdoptAPI: *adopt, DryRun: *dry, Out: os.Stdout}
	// disable never uses the socket, so it works without a runtime directory
	// (for example from su or sudo during uninstall).
	if command == "disable" {
		return userservices.Disable(o)
	}
	socket, err := config.Socket(*socketFlag)
	if err != nil {
		return err
	}
	o.Socket = socket
	switch command {
	case "api-exec":
		return userservices.ExecAPI(socket)
	case "enable":
		shared, err := config.Socket("")
		if err != nil {
			return err
		}
		if shared != socket {
			return errors.New("enable --socket differs from shared configuration; set PODMAN_RUNIT_SOCKET in common.conf before enabling services")
		}
		return userservices.Enable(o)
	case "status", "check-scheduler":
		runtime, err := config.RuntimePath(socket)
		if err != nil {
			return err
		}
		s, err := scheduler.ReadStatus(runtime)
		if err != nil {
			return err
		}
		fresh := time.Since(s.Updated) < 30*time.Second && s.PID > 0 && syscall.Kill(s.PID, 0) == nil && s.State == "ready"
		if command == "status" {
			b, _ := json.MarshalIndent(s, "", "  ")
			fmt.Println(string(b))
		}
		if !fresh {
			return errors.New("scheduler is not ready (stale, stopped, waiting, synchronizing or degraded)")
		}
		return nil
	case "doctor", "check-api":
		c := podmanapi.New(socket)
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if command == "doctor" {
			self, _ := os.Executable()
			fmt.Printf("podman-runit helper: %s (0.1.0)\n", self)
			fmt.Printf("socket: %s\n", socket)
			// Process names are enough to flag likely duplicates without printing argv
			// (which can contain secrets). This cannot identify an arbitrary scheduler.
			matches, _ := filepath.Glob("/proc/[0-9]*/comm")
			count := 0
			for _, path := range matches {
				st, err := os.Stat(path)
				if err != nil {
					continue
				}
				owner, ok := st.Sys().(*syscall.Stat_t)
				if !ok || owner.Uid != uint32(os.Getuid()) {
					continue
				}
				b, err := os.ReadFile(path)
				if err == nil && strings.TrimSpace(string(b)) == "podman-healthch" {
					count++
				}
			}
			fmt.Printf("same-user scheduler processes: %d (multiple endpoints may explain more than one)\n", count)
			binary, _ := exec.LookPath("podman")
			fmt.Printf("podman binary: %s\n", binary)
			if b, err := exec.Command("xbps-query", "-o", binary).Output(); err == nil {
				fmt.Printf("package ownership: %s", b)
			}
			for _, name := range userservices.Names {
				p := filepath.Join(dir, name)
				fmt.Printf("service %s managed=%t\n", p, userservices.Owned(p))
				if b, err := exec.Command("sv", "status", p).Output(); err == nil {
					fmt.Print(string(b))
				}
			}
			runtime, err := config.RuntimePath(socket)
			if err == nil {
				lock, e := os.OpenFile(filepath.Join(runtime, "lock"), os.O_RDWR, 0)
				if e == nil {
					e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
				}
				if e == nil {
					lock.Close()
					fmt.Println("socket scheduler lock: free")
				} else {
					if lock != nil {
						lock.Close()
					}
					if os.IsNotExist(e) {
						fmt.Println("socket scheduler lock: absent")
					} else if errors.Is(e, syscall.EWOULDBLOCK) {
						fmt.Println("socket scheduler lock: held")
					} else {
						fmt.Printf("socket scheduler lock: unavailable (%v)\n", e)
					}
				}
				if s, e := scheduler.ReadStatus(runtime); e == nil {
					fmt.Printf("scheduler: %s updated=%s pid=%d\n", s.State, s.Updated.Format(time.RFC3339), s.PID)
				}
			}
			fmt.Println("Single owner required: path aliases and other schedulers are not excluded by this lock. Services must remain running after logout (configure lingering separately).")
		}
		v, err := c.Version(ctx)
		if err != nil {
			return err
		}
		info, err := c.Info(ctx)
		if err != nil {
			return err
		}
		if info.Host.EventLogger != "file" && info.Host.EventLogger != "journald" {
			return errors.New("configure events_logger=file in containers.conf")
		}
		if !info.Host.Security.Rootless {
			return errors.New("rootful engine unsupported")
		}
		if command == "doctor" {
			fmt.Printf("engine=%s native=%s selected=%s compat=%s events=%s rootless=%t\n", v.Version, v.Native, podmanapi.NativeVersion, v.ApiVersion, info.Host.EventLogger, info.Host.Security.Rootless)
			ids, err := c.List(ctx)
			if err != nil {
				return err
			}
			for _, id := range ids {
				v, err := c.Inspect(ctx, id)
				if err != nil {
					return err
				}
				_, _, why := scheduler.Timing(v)
				if why != "" {
					fmt.Printf("%s: %s\n", id, why)
				}
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}
