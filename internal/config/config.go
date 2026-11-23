package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
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
	return endpointDir(root, socket), nil
}

func endpointDir(root, socket string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(socket)))
	return filepath.Join(root, "podman-runit", hex.EncodeToString(sum[:12]))
}

// SchedulerPath depends only on the UID and socket. Keep the established
// location for endpoints under the standard per-user runtime directory.
func SchedulerPath(socket string) (string, error) {
	uid := strconv.Itoa(os.Getuid())
	root := filepath.Join("/run/user", uid)
	if !strings.HasPrefix(filepath.Clean(socket), root+"/") {
		account, err := user.LookupId(uid)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(account.HomeDir) {
			return "", errors.New("account home directory must be absolute")
		}
		root = filepath.Join(account.HomeDir, ".local/state")
	}
	return endpointDir(root, socket), nil
}

func privateEndpointDir(dir string) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	if err := PrivateDir(parent); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	return PrivateDir(dir)
}

func Runtime(socket string) (string, error) {
	dir, err := RuntimePath(socket)
	if err != nil {
		return "", err
	}
	return dir, privateEndpointDir(dir)
}

// LockSchedulerDir reuses a legacy lock if the two paths name the same inode
// (for example, /var/run and /run). A second flock would conflict with ours.
func LockSchedulerDir(dir string, legacy *os.File) (*os.File, error) {
	if err := privateEndpointDir(dir); err != nil {
		return nil, err
	}
	if legacy != nil {
		held, err := legacy.Stat()
		if err != nil {
			return nil, err
		}
		target, err := os.Lstat(filepath.Join(dir, SchedulerLock))
		if err == nil && os.SameFile(held, target) {
			return legacy, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return Lock(dir)
}

func Lock(dir string) (*os.File, error) {
	fd, err := syscall.Open(filepath.Join(dir, SchedulerLock), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), SchedulerLock)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("scheduler already owns this socket: %w", err)
	}
	return f, nil
}

// SchedulerLock is the scheduler's lock file in the socket's runtime directory.
const SchedulerLock = "lock"

// Holder is a process with an open file that holds a flock.
type Holder struct {
	PID int
	Exe string
}

func (h Holder) String() string { return fmt.Sprintf("pid %d (%s)", h.PID, h.Exe) }

// LockHolders lists same-user processes holding a flock on path, without
// taking the lock. Unreadable processes are skipped.
func LockHolders(path string) ([]Holder, error) {
	target, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	fds, _ := filepath.Glob("/proc/[0-9]*/fd/[0-9]*")
	var holders []Holder
	seen := map[int]bool{}
	for _, fd := range fds {
		pid, _ := strconv.Atoi(strings.Split(fd, "/")[2])
		if seen[pid] {
			continue
		}
		if s, err := os.Stat(fd); err != nil || !os.SameFile(target, s) {
			continue
		}
		info, err := os.ReadFile(strings.Replace(fd, "/fd/", "/fdinfo/", 1))
		if err != nil || !strings.Contains(string(info), "FLOCK") {
			continue
		}
		seen[pid] = true
		exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		holders = append(holders, Holder{pid, exe})
	}
	return holders, nil
}

// Descends reports whether pid is ancestor or one of its descendants.
func Descends(pid, ancestor int) bool {
	for n := 0; n < 64 && pid > 1; n++ {
		if pid == ancestor {
			return true
		}
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			return false
		}
		_, after, _ := strings.Cut(string(b), "\nPPid:")
		ppid, _ := strconv.Atoi(strings.TrimSpace(strings.SplitN(after, "\n", 2)[0]))
		pid = ppid
	}
	return false
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
