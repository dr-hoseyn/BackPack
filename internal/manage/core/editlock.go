package core

import (
	"fmt"
	"path/filepath"

	"github.com/backpack/backpack/internal/app"
)

// LockTunnelConfig excludes other management operations for the tunnel name.
func LockTunnelConfig(name string) (func(), error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	return lockConfigFile(app.ConfigPath(name) + ".edit.lock")
}

func lockRenameConfigs(dir, oldName, newName string) (func(), error) {
	releaseOld, err := lockConfigFile(filepath.Join(dir, oldName+".toml.edit.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock source tunnel %s: %w", oldName, err)
	}
	releaseNew, err := lockConfigFile(filepath.Join(dir, newName+".toml.edit.lock"))
	if err != nil {
		releaseOld()
		return nil, fmt.Errorf("lock destination tunnel %s: %w", newName, err)
	}
	return func() {
		releaseNew()
		releaseOld()
	}, nil
}
