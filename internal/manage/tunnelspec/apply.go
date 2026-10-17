package tunnelspec

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/app"
	"github.com/backpack/backpack/internal/enginectl"
	"github.com/backpack/backpack/internal/manage/core"
	"github.com/backpack/backpack/internal/manage/host"
)

// Apply writes a changed tunnel config and verifies startup. Failed startup
// restores the previous config and service state; errors identify a recovery
// failure rather than reporting a rollback that did not complete.
func Apply(s Spec) error { return ApplyReviewed(s, "") }

// ApplyReviewed also refuses to overwrite a configuration changed since review.
func ApplyReviewed(s Spec, revision string) error {
	if err := s.validateHelpers(); err != nil {
		return err
	}
	return applyWith(s.Name, []byte(s.Render()), "tunnel edit", revision, func() error {
		if err := core.WriteUnit(s.Name); err != nil {
			return err
		}
		return core.DaemonReload()
	})
}

func ApplyBody(name, body, note string) error { return applyRaw(name, []byte(body), note) }

func ApplyBodyReviewed(name, body, note, revision string) error {
	return applyWith(name, []byte(body), note, revision, nil)
}

func applyRaw(name string, body []byte, note string) error {
	return applyWith(name, body, note, "", nil)
}

var editLocks sync.Map

func applyWith(name string, body []byte, note, revision string, prepare func() error) error {
	if err := core.CheckName(name); err != nil {
		return err
	}
	lock, _ := editLocks.LoadOrStore(name, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	path, service := app.ConfigPath(name), app.ServiceName(name)
	tx := editTransaction{
		read:  func() ([]byte, error) { return os.ReadFile(path) },
		write: func(b []byte) error { return app.WriteFileAtomic(path, b, app.TunnelConfigMode) },
		prepare: func() error {
			if err := prepareEditResources(name, body); err != nil {
				return err
			}
			if prepare != nil {
				return prepare()
			}
			return nil
		},
		active:  func() bool { return core.IsActive(service) },
		restart: func() error { return core.RestartService(service) },
		stop:    func() error { return core.StopService(service) },
		ready:   func(b []byte) bool { return waitTunnelActive(name, b) },
		record:  func(b []byte, note string) { RecordChange(name, b, note) },
	}
	return tx.apply(body, note, revision)
}

// Planning a TLS change only names these paths. Generate missing managed
// certificates after the user accepts, never while displaying a preview.
func prepareEditResources(name string, body []byte) error {
	var cfg config.Config
	if _, err := toml.Decode(string(body), &cfg); err != nil {
		return err
	}
	s := cfg.Server
	if s.BindAddr == "" || (s.Transport != "wss" && s.Transport != "wssmux") {
		return nil
	}
	base := app.ConfigDir + "/certs/" + name
	if s.TLSCertFile == base+".crt" && s.TLSKeyFile == base+".key" {
		_, _, err := host.EnsureSelfSignedCert(name, s.ACMEDomain)
		return err
	}
	return nil
}

func PreviewBody(name, body string) (EditPreview, error) {
	if err := core.CheckName(name); err != nil {
		return EditPreview{}, err
	}
	before, err := os.ReadFile(app.ConfigPath(name))
	if err != nil {
		return EditPreview{}, err
	}
	return PreviewChange(before, []byte(body))
}

// Type=simple becomes active before the engine starts its helper. Managed
// carriers also need an affirmative readiness response from the new process;
// neither peer connectivity nor an older engine's missing field can replace it.
func waitTunnelActive(name string, body []byte) bool {
	var cfg config.Config
	if _, err := toml.Decode(string(body), &cfg); err != nil {
		return false
	}
	managed := cfg.Server.Managed() || cfg.Client.Managed() || config.SingDatagram(cfg.L3.Carrier)
	timeout := 10 * time.Second
	if managed {
		// Preflight and listener readiness each have a ten-second bound.
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if !core.WaitServiceActive(app.ServiceName(name), timeout) {
		return false
	}
	return !managed || waitHelperReady(ctx, name)
}

func waitHelperReady(ctx context.Context, name string) bool {
	for ctx.Err() == nil {
		probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		var status enginectl.Status
		err := enginectl.AskContext(probeCtx, name, enginectl.OpStatus, &status)
		cancel()
		if err == nil && status.Name == name && status.HelperReady != nil && *status.HelperReady {
			return true
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	return false
}

// Revert restores a previous config file and brings the service back to the
// state it was in before the edit.
func Revert(path string, prev []byte, service string, wasActive bool) {
	_ = app.WriteFileAtomic(path, prev, app.TunnelConfigMode)
	if wasActive {
		_ = core.RestartService(service)
	} else {
		_ = core.StopService(service)
	}
}

// LastLogLine returns the most recent meaningful journal line for a service,
// used to explain why an edit was reverted.
func LastLogLine(service string) string {
	out, err := exec.Command("journalctl", "-u", service, "-n", "12", "--no-pager", "-o", "cat").Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		low := strings.ToLower(l)
		if strings.Contains(low, "error") || strings.Contains(low, "fatal") ||
			strings.Contains(low, "failed") || strings.Contains(low, "in use") {
			return l
		}
	}
	return ""
}
