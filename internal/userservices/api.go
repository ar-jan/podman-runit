package userservices

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"podman-runit/internal/config"
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
	fd, err := syscall.Open(filepath.Join(dir, "api.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("API endpoint already managed: %w", err)
	}
	if err := prepareSocket(socket); err != nil {
		return err
	}
	return syscall.Exec("/usr/bin/podman", []string{"podman", "system", "service", "--time=0", "unix://" + socket}, os.Environ())
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
