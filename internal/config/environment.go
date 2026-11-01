package config

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// ImportCommon is only used by the on-demand helper. Daemons receive their
// environment from the service launcher, before deriving paths.
func ImportCommon() error {
	if os.Getenv("PODMAN_RUNIT_CONFIG_LOADED") == "1" {
		return nil
	}
	importEnv := func(cmd *exec.Cmd) error {
		b, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("cannot load trusted environment: %w", err)
		}
		for _, entry := range bytes.Split(b, []byte{0}) {
			key, value, ok := strings.Cut(string(entry), "=")
			if ok {
				if err := os.Setenv(key, value); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if dir := os.Getenv("TURNSTILE_ENV_DIR"); dir != "" && os.Getenv("PODMAN_RUNIT_ENV_LOADED") != "1" {
		if err := importEnv(exec.Command("chpst", "-e", dir, "env", "-0")); err != nil {
			return err
		}
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".config")
	}
	file := filepath.Join(base, "podman-runit/common.conf")
	s, err := os.Stat(file)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	st, ok := s.Sys().(*syscall.Stat_t)
	if !ok || !s.Mode().IsRegular() || st.Uid != uint32(os.Getuid()) || s.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("untrusted config: %s", file)
	}
	return importEnv(exec.Command("/bin/sh", "-c", `set -a; . "$1"; exec env -0`, "podman-runit-config", file))
}
