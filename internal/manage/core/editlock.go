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
	return lockTunnelConfig(app.ConfigDir, name)
}

// LockConfigSnapshot excludes managed tunnel changes while the configuration
// tree is copied or restored.
func LockConfigSnapshot() (func(), error) {
	return lockConfigTree(app.ConfigDir, true)
}

func lockTunnelConfig(dir, name string) (func(), error) {
	releaseTree, err := lockConfigTree(dir, false)
	if err != nil {
		return nil, err
	}
	releaseFile, err := lockConfigFile(filepath.Join(dir, name+".toml.edit.lock"))
	if err != nil {
		releaseTree()
		return nil, err
	}
	return func() {
		releaseFile()
		releaseTree()
	}, nil
}

func lockRenameConfigs(dir, oldName, newName string) (func(), error) {
	releaseTree, err := lockConfigTree(dir, false)
	if err != nil {
		return nil, err
	}
	releaseOld, err := lockConfigFile(filepath.Join(dir, oldName+".toml.edit.lock"))
	if err != nil {
		releaseTree()
		return nil, fmt.Errorf("lock source tunnel %s: %w", oldName, err)
	}
	releaseNew, err := lockConfigFile(filepath.Join(dir, newName+".toml.edit.lock"))
	if err != nil {
		releaseOld()
		releaseTree()
		return nil, fmt.Errorf("lock destination tunnel %s: %w", newName, err)
	}
	return func() {
		releaseNew()
		releaseOld()
		releaseTree()
	}, nil
}
