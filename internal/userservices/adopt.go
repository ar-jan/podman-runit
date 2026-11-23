package userservices

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// adoptService keeps runsv's service directory and supervision data in place.
// Prepare every replacement before stopping the service. Each rename has an
// inverse until the new service passes its up check.
func adoptService(dest, stage string) (err error) {
	install := []string{"run", "check", Marker}
	for _, name := range []string{"conf", "log", Marker} {
		path := filepath.Join(dest, name)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			if name != Marker {
				install = append(install, name)
			}
			continue
		} else if err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if name == Marker || (name == "log" && !info.IsDir()) || (name == "conf" && !info.Mode().IsRegular()) {
			return fmt.Errorf("cannot adopt service with conflicting %s", path)
		}
	}

	live, err := supervised(dest)
	if err != nil {
		return err
	}
	wantUp := false
	if live {
		status, err := os.ReadFile(filepath.Join(dest, "supervise/status"))
		if err != nil {
			return err
		}
		if len(status) < 18 {
			return fmt.Errorf("incomplete runsv status in %s", dest)
		}
		wantUp = status[17] == 'u'
	}

	backup, err := os.MkdirTemp(dest, ".before-adoption-")
	if err != nil {
		return err
	}
	type rename struct{ from, to string }
	var moved []rename
	move := func(from, to string) error {
		if err := os.Rename(from, to); err != nil {
			return err
		}
		moved = append(moved, rename{from, to})
		return nil
	}
	var stopped, activating bool
	defer func() {
		if err == nil {
			return
		}
		if activating {
			err = errors.Join(err, control(dest, "down"))
		}
		for i := len(moved) - 1; i >= 0; i-- {
			r := moved[i]
			err = errors.Join(err, os.Rename(r.to, r.from))
		}
		// A failed restore leaves the original in the backup for recovery.
		err = errors.Join(err, os.Remove(backup))
		if stopped && wantUp {
			err = errors.Join(err, control(dest, "up"))
		}
	}()

	down := filepath.Join(dest, "down")
	savedDown := filepath.Join(backup, "down")
	if _, err := os.Lstat(down); os.IsNotExist(err) {
		savedDown = filepath.Join(stage, "down")
		if err := move(savedDown, down); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	stopped = true
	if err := control(dest, "down"); err != nil {
		return err
	}
	for _, name := range []string{"run", "check"} {
		path := filepath.Join(dest, name)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := move(path, filepath.Join(backup, name)); err != nil {
			return err
		}
	}
	for _, name := range install {
		if err := move(filepath.Join(stage, name), filepath.Join(dest, name)); err != nil {
			return err
		}
	}
	if err := move(down, savedDown); err != nil {
		return err
	}
	activating = true
	return control(dest, "up")
}
