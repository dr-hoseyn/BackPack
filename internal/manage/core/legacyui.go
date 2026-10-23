package core

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/backpack/backpack/internal/app"
)

// RetireLegacyPanel removes the service installed by releases with a web UI.
// Old settings and backups remain inert files; no code reads their credentials.
func RetireLegacyPanel() error { return retireLegacyPanel(app.ServiceDir, Systemctl) }

func retireLegacyPanel(dir string, run func(...string) (string, error)) error {
	const unit = "backpack-webui.service"
	path := filepath.Join(dir, unit)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := run("disable", "--now", unit); err != nil {
		return fmt.Errorf("stop the retired web service: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if _, err := run("daemon-reload"); err != nil {
		return err
	}
	unitCache.Forget()
	return nil
}
