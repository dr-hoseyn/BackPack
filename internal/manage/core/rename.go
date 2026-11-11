package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/backpack/backpack/internal/app"
	"github.com/backpack/backpack/internal/metrics"
	"github.com/backpack/backpack/internal/quota"
)

// Renaming a tunnel.
//
// A tunnel's name is its file's name, its unit's name, and the key everything
// kept about it is filed under: the traffic it carried, the configurations it
// had, its label, its restart schedule, its pairing with a node. A rename has
// to move all of them, or the tunnel comes back as a stranger to its own
// history. The layers above this one move what is theirs through OnRename.

var onRename []func(oldName, newName string)

// OnRename registers what a layer above has to move when a tunnel is renamed.
func OnRename(f func(oldName, newName string)) { onRename = append(onRename, f) }

// Rename gives a tunnel a new name and brings it back up under it, running if
// it was running.
func Rename(oldName, newName string) error {
	if err := CheckName(oldName); err != nil {
		return err
	}
	if err := CheckName(newName); err != nil {
		return err
	}
	if oldName == newName {
		return nil
	}
	release, err := lockRenameConfigs(app.ConfigDir, oldName, newName)
	if err != nil {
		return err
	}
	defer release()
	if err := checkRenameFiles(app.ConfigDir, oldName, newName); err != nil {
		return err
	}

	oldService, newService := app.ServiceName(oldName), app.ServiceName(newName)
	newUnit := filepath.Join(app.ServiceDir, newService)
	if _, err := os.Lstat(newUnit); err == nil {
		return fmt.Errorf("systemd unit %s already exists", newService)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check systemd unit %s: %w", newService, err)
	}
	active, enabled := IsActive(oldService), IsEnabled(oldService)
	hours, minute := ScheduledRestart(oldName)

	// Stopped first, so its last traffic count is written under the old name
	// before the file moves.
	if active || enabled {
		if err := DisableService(oldService); err != nil {
			return fmt.Errorf("stop %s before renaming: %w", oldService, err)
		}
	}
	restoreOld := func(why error) error {
		err := DaemonReload()
		if hours > 0 {
			err = errors.Join(err, SetScheduledRestart(oldName, hours, minute))
		}
		return errors.Join(why, err, restoreRenameService(oldService, active, enabled, Systemctl))
	}

	if err := removeScheduledRestart(oldName); err != nil {
		return restoreOld(fmt.Errorf("remove old restart schedule: %w", err))
	}

	if err := moveRenameFiles(app.ConfigDir, oldName, newName, func() error {
		if err := app.WriteFileAtomic(newUnit, []byte(UnitFor(newName)), 0644); err != nil {
			return fmt.Errorf("write new systemd unit: %w", err)
		}
		if err := DaemonReload(); err != nil {
			removeErr := os.Remove(newUnit)
			return errors.Join(fmt.Errorf("load new systemd unit: %w", err), removeErr)
		}
		return nil
	}); err != nil {
		return restoreOld(err)
	}

	var cleanupErr error
	if err := os.Remove(filepath.Join(app.ServiceDir, oldService)); err != nil && !os.IsNotExist(err) {
		cleanupErr = fmt.Errorf("renamed, but could not remove the previous systemd unit: %w", err)
	}
	cleanupErr = errors.Join(cleanupErr, DaemonReload())
	renameTunnelMeta(oldName, newName)
	for _, f := range onRename {
		f(oldName, newName)
	}

	var scheduleErr error
	if hours > 0 {
		if err := SetScheduledRestart(newName, hours, minute); err != nil {
			scheduleErr = fmt.Errorf("renamed, but the restart schedule could not be installed: %w", err)
		}
	}
	if err := restoreRenameService(newService, active, enabled, Systemctl); err != nil {
		return errors.Join(cleanupErr, scheduleErr, fmt.Errorf("renamed, but could not restore %s service state: %w", newService, err))
	}
	return errors.Join(cleanupErr, scheduleErr)
}

// moveRenameFiles restores the files it moved if a later move or systemd
// preparation fails. Optional counters and quota may be absent.
func moveRenameFiles(dir, oldName, newName string, prepareUnit func() error) (err error) {
	type move struct {
		from, to string
		optional bool
	}
	files := []move{
		{filepath.Join(dir, oldName+".toml"), filepath.Join(dir, newName+".toml"), false},
		{quota.Path(dir, oldName), quota.Path(dir, newName), true},
		{metrics.Path(dir, oldName), metrics.Path(dir, newName), true},
	}
	var moved []move
	defer func() {
		if err == nil {
			return
		}
		for i := len(moved) - 1; i >= 0; i-- {
			f := moved[i]
			if restoreErr := os.Rename(f.to, f.from); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restore %s: %w", f.from, restoreErr))
			}
		}
	}()
	for _, f := range files {
		if _, statErr := os.Lstat(f.to); statErr == nil {
			return fmt.Errorf("destination %s already exists", f.to)
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		if moveErr := os.Rename(f.from, f.to); moveErr != nil {
			if f.optional && os.IsNotExist(moveErr) {
				continue
			}
			return moveErr
		}
		moved = append(moved, f)
	}
	return prepareUnit()
}

func checkRenameFiles(dir, oldName, newName string) error {
	source := filepath.Join(dir, oldName+".toml")
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("read configuration for %q: %w", oldName, err)
	}
	for _, candidate := range []struct {
		path, kind string
	}{
		{filepath.Join(dir, newName+".toml"), "configuration"},
		{metrics.Path(dir, newName), "traffic counters"},
		{quota.Path(dir, newName), "traffic quota"},
	} {
		if _, err := os.Lstat(candidate.path); err == nil {
			return fmt.Errorf("%s for %q already exists", candidate.kind, newName)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("check %s for %q: %w", candidate.kind, newName, err)
		}
	}
	return nil
}

func restoreRenameService(service string, active, enabled bool, systemctl func(...string) (string, error)) error {
	defer unitCache.Forget()
	var err error
	if enabled {
		_, err = systemctl("enable", service)
	}
	if active {
		_, startErr := systemctl("start", service)
		err = errors.Join(err, startErr)
	}
	return err
}

// renameTunnelMeta moves a tunnel's metadata to its new name.
func renameTunnelMeta(oldName, newName string) {
	metaMu.Lock()
	defer metaMu.Unlock()
	m := loadMeta()
	if e, ok := m[oldName]; ok {
		m[newName] = e
		delete(m, oldName)
		saveMeta(m)
	}
}
