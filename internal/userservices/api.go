package userservices

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"podman-runit/internal/config"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ExecAPI retains a kernel lock across exec. It never removes a live socket.
// A refused, same-user socket in a directory only the user can write may be
// left by a killed managed API process; only that stale inode is removed under
// the API lock.
func ExecAPI(socket string) error {
	if os.Geteuid() == 0 {
		return errors.New("rootful API service is outside the supported scope")
	}
	dir, err := config.Runtime(socket)
	if err != nil {
		return err
	}
	fd, err := lockAPI(dir, socket)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err := prepareSocket(socket); err != nil {
		return err
	}
	return syscall.Exec("/usr/bin/podman", []string{"podman", "system", "service", "--time=0", "unix://" + socket}, os.Environ())
}

// lockAPI takes the endpoint lock, deliberately without close-on-exec so that
// the API process holds it. A busy lock names its holders.
func lockAPI(dir, socket string) (int, error) {
	path := filepath.Join(dir, APILock)
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return -1, err
	}
	err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return fd, nil
	}
	syscall.Close(fd)
	holders, _ := config.LockHolders(path)
	if len(holders) == 0 {
		return -1, fmt.Errorf("API endpoint already managed: %w", err)
	}
	// On first start rootless Podman serves from a forked child that outlives
	// a SIGKILLed parent and keeps this lock, so runit cannot restart the API.
	return -1, fmt.Errorf("API endpoint already managed by %s; if no other service manages %s, that API outlived its supervisor (for example after sv kill): stop it with kill %s", describeHolders(holders), socket, holderPIDs(holders))
}

// APILock is the endpoint lock file in the socket's runtime directory.
const APILock = "api.lock"

func describeHolders(holders []config.Holder) string {
	parts := make([]string, len(holders))
	for i, h := range holders {
		parts[i] = h.String()
	}
	return strings.Join(parts, ", ")
}
func holderPIDs(holders []config.Holder) string {
	pids := make([]string, len(holders))
	for i, h := range holders {
		pids[i] = strconv.Itoa(h.PID)
	}
	return strings.Join(pids, " ")
}

// SupervisedPID returns the process runsv supervises for a service, or 0.
func SupervisedPID(service string) int {
	b, err := os.ReadFile(filepath.Join(service, "supervise/pid"))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

func prepareSocket(socket string) error {
	parent := filepath.Dir(socket)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	if err := config.SocketDir(parent); err != nil {
		return err
	}
	if before, err := os.Lstat(socket); err == nil {
		st, ok := before.Sys().(*syscall.Stat_t)
		if !ok || before.Mode()&os.ModeSocket == 0 || st.Uid != uint32(os.Getuid()) {
			return errors.New("refusing non-socket or foreign socket")
		}
		conn, err := net.DialTimeout("unix", socket, time.Second)
		if err == nil {
			conn.Close()
			return errors.New("socket already served; reuse existing API with --scheduler-only")
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return fmt.Errorf("socket not demonstrably stale: %w", err)
		}
		after, err := os.Lstat(socket)
		if err != nil {
			return err
		}
		if !os.SameFile(before, after) {
			return errors.New("socket changed while checking ownership")
		}
		if err := os.Remove(socket); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}
