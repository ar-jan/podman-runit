package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func PrivateDir(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("runtime directory must be absolute")
	}
	if ok, err := ownedDir(path, 0077); err != nil || !ok {
		return orError(err, "runtime directory must be a private, user-owned directory (0700)")
	}
	return nil
}

// SocketDir accepts a user-owned directory that only its owner can write, so no
// other user can replace a socket in it. Others may still traverse it (0755).
func SocketDir(path string) error {
	if ok, err := ownedDir(path, 0022); err != nil || !ok {
		return orError(err, "socket directory must be user owned and writable only by that user")
	}
	return nil
}
func ownedDir(path string, forbidden os.FileMode) (bool, error) {
	s, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	st, ok := s.Sys().(*syscall.Stat_t)
	return ok && s.IsDir() && st.Uid == uint32(os.Getuid()) && s.Mode().Perm()&forbidden == 0, nil
}
func orError(err error, message string) error {
	if err != nil {
		return err
	}
	return errors.New(message)
}
func Socket(explicit string) (string, error) {
	if explicit == "" {
		explicit = os.Getenv("PODMAN_RUNIT_SOCKET")
	}
	if explicit == "" {
		r := os.Getenv("XDG_RUNTIME_DIR")
		if err := PrivateDir(r); err != nil {
			return "", err
		}
		explicit = filepath.Join(r, "podman/podman.sock")
	}
	explicit = strings.TrimPrefix(explicit, "unix://")
	if !filepath.IsAbs(explicit) || strings.Contains(explicit, "://") {
		return "", errors.New("socket must be an absolute Unix path")
	}
	return filepath.Clean(explicit), nil
}
func RuntimePath(socket string) (string, error) {
	root := os.Getenv("XDG_RUNTIME_DIR")
	if err := PrivateDir(root); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(socket))
	dir := filepath.Join(root, "podman-runit", hex.EncodeToString(sum[:12]))
	return dir, nil
}
func Runtime(socket string) (string, error) {
	dir, err := RuntimePath(socket)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := PrivateDir(filepath.Dir(dir)); err != nil {
		return "", err
	}
	return dir, PrivateDir(dir)
}
func Lock(dir string) (*os.File, error) {
	fd, err := syscall.Open(filepath.Join(dir, "lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "lock")
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("scheduler already owns this socket: %w", err)
	}
	return f, nil
}
func Atomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".write-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
