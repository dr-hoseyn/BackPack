package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/backpack/backpack/internal/app"
	"github.com/backpack/backpack/internal/manage/core"
)

// snapshotRetention is how many pre-update snapshots are kept on disk.
const snapshotRetention = 5

// SnapshotMeta describes one snapshot, stored alongside it as meta.json.
type SnapshotMeta struct {
	Stamp   string   `json:"stamp"`   // 20260714-2210
	Version string   `json:"version"` // the version that was running when taken
	Created string   `json:"created"` // RFC3339, optionally with fractional seconds
	Reason  string   `json:"reason"`  // e.g. "pre-update"
	Tunnels []string `json:"tunnels"` // tunnel names captured
}

// Snapshot is a restorable point-in-time copy of the binary and all configs.
type Snapshot struct {
	Dir  string
	Meta SnapshotMeta
}

// Root is where the update snapshots live. Exported because the diagnostics
// list it among the directories this product owns.
func Root() string { return app.InstallDir + "/snapshots" }

// TakeSnapshot copies the running binary and the whole config directory into a
// timestamped folder so a failed update or a bad config change can be undone.
func TakeSnapshot(reason string) (Snapshot, error) {
	release, err := core.LockConfigSnapshot()
	if err != nil {
		return Snapshot{}, err
	}
	defer release()
	now := time.Now()
	stamp := now.Format("20060102-150405")
	if err := os.MkdirAll(Root(), 0755); err != nil {
		return Snapshot{}, err
	}
	dir, err := os.MkdirTemp(Root(), reason+"-"+stamp+"-")
	if err != nil {
		return Snapshot{}, err
	}

	// 1) The binary currently installed.
	if core.FileExists(app.BinPath) {
		if err := copyFile(app.BinPath, filepath.Join(dir, "backpack"), 0755); err != nil {
			os.RemoveAll(dir)
			return Snapshot{}, fmt.Errorf("could not snapshot the binary: %w", err)
		}
	}

	// 2) Every config, token, cert and unit-relevant file.
	if err := copyTree(app.ConfigDir, filepath.Join(dir, "config")); err != nil {
		os.RemoveAll(dir)
		return Snapshot{}, fmt.Errorf("could not snapshot the configuration: %w", err)
	}

	var names []string
	for _, t := range core.List() {
		names = append(names, t.Name)
	}
	meta := SnapshotMeta{
		Stamp:   stamp,
		Version: app.Version,
		Created: now.UTC().Format(time.RFC3339Nano),
		Reason:  reason,
		Tunnels: names,
	}
	data, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), data, 0600); err != nil {
		os.RemoveAll(dir)
		return Snapshot{}, err
	}

	pruneSnapshots()
	return Snapshot{Dir: dir, Meta: meta}, nil
}

// ListSnapshots returns every snapshot, newest first.
func ListSnapshots() []Snapshot {
	return listSnapshots(Root())
}

func listSnapshots(root string) []Snapshot {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []Snapshot
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		s := Snapshot{Dir: dir}
		data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if err != nil || json.Unmarshal(data, &s.Meta) != nil || s.Meta.Stamp == "" {
			// In-progress and interrupted copies are not restore points.
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, out[i].Meta.Created)
		b, _ := time.Parse(time.RFC3339Nano, out[j].Meta.Created)
		if !a.Equal(b) {
			return a.After(b)
		}
		return out[i].Dir > out[j].Dir
	})
	return out
}

// pruneSnapshots keeps only the newest snapshotRetention snapshots.
func pruneSnapshots() {
	all := ListSnapshots()
	for i := snapshotRetention; i < len(all); i++ {
		os.RemoveAll(all[i].Dir)
	}
}

// RestoreSnapshot puts the binary and configs from a snapshot back in place,
// re-registers a unit for every tunnel it contains and restarts everything.
func RestoreSnapshot(s Snapshot, logf func(string)) error {
	if logf == nil {
		logf = func(string) {}
	}
	if s.Dir == "" || !core.FileExists(filepath.Join(s.Dir, "meta.json")) {
		return fmt.Errorf("snapshot not found or incomplete: %s", s.Dir)
	}

	release, err := core.LockConfigSnapshot()
	if err != nil {
		return err
	}
	defer release()

	// Prepare the entire configuration before changing either live artifact.
	var stage string
	if cfg := filepath.Join(s.Dir, "config"); core.FileExists(cfg) {
		logf("Preparing restored configuration...")
		stage, err = stageSnapshotConfig(cfg, app.ConfigDir)
		if err != nil {
			return fmt.Errorf("could not stage the configuration: %w", err)
		}
		defer os.RemoveAll(stage)
	}

	var activeServices []string
	if stage != "" {
		for _, tunnel := range core.List() {
			if core.IsActive(tunnel.Service) {
				activeServices = append(activeServices, tunnel.Service)
			}
		}
	}
	if err := commitStoppedRestore(activeServices, core.StopService, core.RestartService, func() error {
		installConfig := func() error {
			if stage != "" {
				logf("Restoring configuration...")
				return commitRestore(app.ConfigDir, stage)
			}
			return nil
		}
		if bin := filepath.Join(s.Dir, "backpack"); core.FileExists(bin) {
			logf("Restoring the previous binary...")
			return installSnapshotBinary(bin, app.BinPath, installConfig)
		}
		return installConfig()
	}); err != nil {
		return err
	}

	// 3) Re-register and restart everything.
	logf("Restarting services...")
	var failures []error
	for _, t := range core.List() {
		if err := core.WriteUnit(t.Name); err != nil {
			failures = append(failures, fmt.Errorf("restore service unit %s: %w", t.Name, err))
		}
	}
	if err := core.DaemonReload(); err != nil {
		failures = append(failures, fmt.Errorf("reload restored service units: %w", err))
	}
	// The same order as the update itself, for the same reason: a rollback
	// started from the panel runs inside the panel's unit. See
	// core/restartorder.go.
	ok, failed, later := core.RestartForNewBinary(logf)
	logf(fmt.Sprintf("Restarted %d tunnels (%d failed).", ok, failed))
	if len(later) > 0 {
		logf(strings.Join(later, ", ") + " restarts next — this connection drops for a moment.")
	}
	if failed > 0 {
		failures = append(failures, fmt.Errorf("%d tunnel service(s) failed to restart after rollback", failed))
	}
	return errors.Join(failures...)
}

// Keep the currently installed binary available until the config commit
// succeeds so a failed restore can resume the original configuration.
func installSnapshotBinary(source, target string, commitConfig func() error) error {
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	backupDir, err := os.MkdirTemp(filepath.Dir(target), ".backpack-binary-restore-*")
	if err != nil {
		return err
	}
	keepRecovery := false
	defer func() {
		if !keepRecovery {
			_ = os.RemoveAll(backupDir)
		}
	}()
	previous := filepath.Join(backupDir, "backpack")
	info, statErr := os.Stat(target)
	if statErr != nil && !os.IsNotExist(statErr) {
		return statErr
	}
	if statErr == nil {
		if err := copyFile(target, previous, info.Mode().Perm()); err != nil {
			return fmt.Errorf("save installed binary for restore recovery: %w", err)
		}
	}
	if err := copyFile(source, target, 0755); err != nil {
		return fmt.Errorf("restore snapshot binary: %w", err)
	}
	if err := commitConfig(); err != nil {
		var recovery error
		if statErr == nil {
			recovery = copyFile(previous, target, info.Mode().Perm())
		} else {
			recovery = os.Remove(target)
		}
		if recovery != nil {
			keepRecovery = statErr == nil
			return errors.Join(err, fmt.Errorf("recover installed binary after failed config restore (saved copy in %s): %w", backupDir, recovery))
		}
		return err
	}
	return nil
}

// stageSnapshotConfig merges into a private tree so a failed copy leaves
// the live installation intact.
func stageSnapshotConfig(source, live string) (stage string, err error) {
	if err := os.MkdirAll(live, 0755); err != nil {
		return "", err
	}
	stage, err = os.MkdirTemp(filepath.Dir(live), ".backpack-snapshot-restore-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(stage)
		}
	}()
	if err = seedStage(live, stage); err != nil {
		return stage, err
	}
	err = copyTree(source, stage)
	return stage, err
}

// --- small file helpers -----------------------------------------------------

// copyFile publishes a complete copy with the requested mode. Replacing the
// destination inode also avoids following a destination symlink.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(dst), ".snapshot-copy-*")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Chmod(mode)
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(out.Name(), dst)
}

// copyTree recursively copies a directory, preserving file permissions.
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dst, info.Mode().Perm())
	}
	return filepath.Walk(src, func(path string, fi os.FileInfo, walkErr error) error {
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if isMetricsTemp(rel) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			if existing, err := os.Lstat(target); err == nil && !existing.IsDir() {
				return fmt.Errorf("snapshot directory would replace a non-directory: %s", target)
			} else if err != nil && !os.IsNotExist(err) {
				return err
			}
			return os.MkdirAll(target, 0755)
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		return copyFile(path, target, fi.Mode().Perm())
	})
}
