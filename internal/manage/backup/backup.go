package backup

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/backpack/backpack/internal/app"
	"github.com/backpack/backpack/internal/manage/core"
	"github.com/backpack/backpack/internal/schedule"
)

// backupMetaName is a synthetic entry stored inside the archive (not written to
// disk on restore) that captures settings living outside ConfigDir — currently
// the auto-refresh interval, which is kept in the crontab.
const backupMetaName = ".backpack-backup.json"

// fleetKeyName is the one file in the config directory that is never archived.
// See where it is skipped, below, and internal/node/seal.go for what it is.
const fleetKeyName = "node.key"

// backupMeta is the sidecar metadata embedded in every backup archive.
type backupMeta struct {
	Version          string `json:"version"`
	Created          string `json:"created"`
	AutoRefreshHours *int   `json:"auto_refresh_hours,omitempty"`
	AutoRefreshKnown bool   `json:"auto_refresh_known,omitempty"`
}

// RestoreResult summarises what a restore put back in place.
type RestoreResult struct {
	Files            int      // config files written to disk
	Tunnels          []string // tunnels re-registered as systemd services
	Started          int      // tunnels successfully started
	Failed           int      // tunnels that could not be registered or restarted
	TelegramConfig   bool     // telegram.json was present in the archive
	AutoRefreshHours int      // auto-refresh interval restored from the archive
	AutoRefreshSet   bool     // the schedule was applied, including explicit disable

	// Warnings are the things the restore carried on past. A restore that half
	// worked and said so is recoverable; one that half worked quietly is
	// discovered weeks later, by whatever stopped happening.
	Warnings []string
}

// WriteBackup streams a gzip-compressed tar of the entire config directory.
//
// Tunnel configs, tokens, Telegram settings, certificates and install metadata
// share one archive, plus a sidecar for the auto-refresh schedule.
func WriteBackup(w io.Writer) error {
	release, err := core.LockConfigSnapshot()
	if err != nil {
		return err
	}
	defer release()
	return writeBackupTree(w, app.ConfigDir)
}

// writeBackupTree is WriteBackup against a named root, so that the archive
// itself can be exercised against a tree a test builds rather than against the
// one real installation on the machine.
func writeBackupTree(w io.Writer, root string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	if err := writeBackupEntries(tw, root); err != nil {
		tw.Close()
		gz.Close()
		return err
	}

	// Both of these were deferred, and a deferred Close reports to nobody.
	// They are not cleanup: closing the tar writer writes the archive's footer
	// and closing the gzip writer writes the compressed trailer, so a failure
	// here — a full disk, a browser that hung up — is the difference between a
	// backup and most of one. Reported as an error, the caller deletes the file
	// and says so; discarded, it returns success over a truncated archive that
	// is not found to be short until the day it is needed.
	if err := tw.Close(); err != nil {
		gz.Close()
		return fmt.Errorf("finishing the archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("finishing compression: %w", err)
	}
	return nil
}

// writeBackupEntries puts the sidecar and the whole config tree into tw.
func writeBackupEntries(tw *tar.Writer, root string) error {
	// Sidecar metadata for settings that don't live under ConfigDir.
	hours, scheduleErr := schedule.ReadAutoRefreshHours()
	meta := backupMeta{
		Version: app.Version,
		Created: time.Now().UTC().Format(time.RFC3339),
	}
	if scheduleErr == nil {
		meta.AutoRefreshHours = &hours
		meta.AutoRefreshKnown = true
	}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("describing the backup: %w", err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: backupMetaName,
		Mode: 0600,
		Size: int64(len(metaJSON)),
	}); err != nil {
		return err
	}
	if _, err := tw.Write(metaJSON); err != nil {
		return err
	}

	// Walk the config directory and add every file, preserving relative paths
	// and permissions (so 0600 key/config files stay 0600 on restore).
	return filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if isMetricsTemp(rel) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if rel == "." {
			return nil
		}

		// The fleet's sealing key is deliberately not in the archive.
		//
		// It is the whole of what the sealing is worth. The registry beside it
		// holds the root password of every managed server, and a backup is a
		// thing people move — downloaded through the panel, sent through the
		// bot, kept on a laptop, attached to a support message. Sealed with a
		// key that travels in the same file, that is plaintext with extra
		// steps.
		//
		// Restoring onto the machine that took the backup still works, because
		// a restore seeds its staging tree from the live directory and keeps a
		// file the archive does not mention. Restoring onto a different machine
		// leaves those passwords unreadable, and the fleet screen asks for them
		// again — which is what an operator would choose if they were asked
		// whether a backup should carry them.
		if rel == fleetKeyName {
			return nil
		}

		// Anything that is not a directory or an ordinary file is refused
		// rather than archived. A symlink here produced an entry with no
		// target recorded and no content behind it — neither the link nor the
		// file it pointed at — so the archive looked complete and restored to
		// something that was not. This directory is written by Backpack and
		// holds configs, certificates and JSON; a symlink in it is a surprise,
		// and the time to hear about a surprise in a backup is while it is
		// being taken.
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to back up %s: not a regular file (%s)", rel, info.Mode().Type())
		}

		name := filepath.ToSlash(rel)
		if !info.IsDir() {
			return writeBackupFile(tw, path, name)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = name + "/"
		return tw.WriteHeader(hdr)
	})
}

// Metrics publishers can rename these files while the config is being copied.
func isMetricsTemp(rel string) bool {
	return filepath.Dir(rel) == "." &&
		(strings.HasSuffix(rel, ".metrics.json.tmp") ||
			strings.HasPrefix(rel, ".") && strings.Contains(rel, ".metrics.json-") && strings.HasSuffix(rel, ".tmp"))
}

// Read metadata and content from the same open file. Background collectors can
// atomically replace their snapshots between the directory walk and this open.
func writeBackupFile(tw *tar.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to back up %s: not a regular file", name)
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = name
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("archive %s: %w", name, err)
	}
	return nil
}

// BackupToFile writes a complete archive and prunes old backups in dir.
func BackupToFile(dir string) (string, error) {
	return publishBackup(dir, WriteBackup)
}

// Restore reads a backup archive produced by WriteBackup and puts the
// configuration it holds in place, then re-registers a systemd service for
// every tunnel it finds and starts them. It also restores the auto-refresh
// schedule from the archive sidecar.
//
// Nothing in the live config directory changes until the whole archive has
// been read, checked and staged; see restore.go.
func Restore(r io.Reader) (RestoreResult, error) {
	var res RestoreResult

	release, err := core.LockConfigSnapshot()
	if err != nil {
		return res, err
	}
	defer release()

	if err := os.MkdirAll(app.ConfigDir, 0755); err != nil {
		return res, err
	}

	// Beside the config directory, so the commit is a rename rather than a copy
	// across filesystems — and so a restore cannot half-succeed for want of
	// space somewhere else.
	stage, err := os.MkdirTemp(filepath.Dir(app.ConfigDir), ".backpack-restore-*")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(stage)

	contents, err := stageRestore(r, app.ConfigDir, stage)
	if err != nil {
		// Staging failed, so the live directory was never touched.
		return res, err
	}
	var activeServices []string
	if contents.SawTunnelConfig {
		for _, tunnel := range core.List() {
			if core.IsActive(tunnel.Service) {
				activeServices = append(activeServices, tunnel.Service)
			}
		}
	}
	if err := commitStoppedRestore(activeServices, core.StopService, core.RestartService, func() error {
		return commitRestore(app.ConfigDir, stage)
	}); err != nil {
		return res, err
	}

	res.Files = contents.Files
	res.TelegramConfig = contents.TelegramConfig
	res.Warnings = append(res.Warnings, contents.Warnings...)

	if contents.SawTunnelConfig {
		for _, t := range core.List() {
			if err := core.WriteUnit(t.Name); err != nil {
				res.Failed++
				res.Warnings = append(res.Warnings, fmt.Sprintf("register tunnel %s: %v", t.Name, err))
				continue
			}
			res.Tunnels = append(res.Tunnels, t.Name)
		}
		if len(res.Tunnels) > 0 {
			if err := core.DaemonReload(); err != nil {
				res.Failed += len(res.Tunnels)
				res.Warnings = append(res.Warnings, fmt.Sprintf("reload systemd after registering tunnels: %v", err))
			} else {
				for _, name := range res.Tunnels {
					service := app.ServiceName(name)
					if err := core.RestartService(service); err != nil {
						res.Failed++
						res.Warnings = append(res.Warnings, fmt.Sprintf("restart tunnel %s: %v", name, err))
						continue
					}
					res.Started++
					if output, err := core.Systemctl("enable", service); err != nil {
						res.Warnings = append(res.Warnings, fmt.Sprintf("enable tunnel %s at boot: %v %s", name, err, output))
					}
				}
			}
		}
	}

	if contents.HasAutoRefresh {
		if err := schedule.SetAutoRefresh(contents.AutoRefreshHours); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("restore auto-refresh schedule: %v", err))
		} else {
			res.AutoRefreshHours = contents.AutoRefreshHours
			res.AutoRefreshSet = true
		}
	}

	return res, nil
}
