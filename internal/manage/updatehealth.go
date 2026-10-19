package manage

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/backpack/backpack/internal/app"
	"github.com/backpack/backpack/internal/manage/backup"
	"github.com/backpack/backpack/internal/tunnel/portmap"
	"github.com/backpack/backpack/internal/updatehealth"
)

// This is local policy, kept separate from peer-shared tunnel credentials and
// rendered TOML. Backups already include the whole configuration directory.
func prepareUpdateHealth(logf func(string)) (updatehealth.Policy, error) {
	p, err := updatehealth.Load(filepath.Join(app.ConfigDir, "update-health.json"))
	if err != nil {
		return p, fmt.Errorf("could not load update-health.json: %w", err)
	}
	var installed []Tunnel
	for _, t := range List() {
		if fileExists(filepath.Join(app.ServiceDir, t.Service)) {
			installed = append(installed, t)
		}
	}
	return prepareUpdateHealthWith(p, installed, logf)
}

func prepareUpdateHealthWith(p updatehealth.Policy, installed []Tunnel, logf func(string)) (updatehealth.Policy, error) {
	if err := validateUpdateHealthRoutes(p, installed); err != nil {
		return p, err
	}
	checked := make(map[string]bool)
	for _, c := range p.Checks {
		checked[c.Tunnel] = true
	}
	for _, t := range installed {
		if !checked[t.Name] {
			logf(t.Name + ": no payload check configured; update verifies service startup only (see docs/updates.md).")
		}
	}
	if len(p.Checks) == 0 {
		return p, nil
	}
	logf("Verifying forwarded payloads before installation...")
	// A failed baseline must stop before replacing the binary. Otherwise an
	// existing backend outage would be blamed on every new release.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if bad := updatehealth.Verify(ctx, p.Checks, false); len(bad) != 0 {
		return p, fmt.Errorf("update not installed: baseline payload check failed for %s; repair the existing path or endpoint first", reportUpdateHealthFailures(bad, logf))
	}
	logf(fmt.Sprintf("Baseline payload verified for %d tunnel(s).", len(p.Checks)))
	return p, nil
}

func validateUpdateHealthRoutes(p updatehealth.Policy, tunnels []Tunnel) error {
	if err := p.Validate(); err != nil {
		return err
	}
	for _, c := range p.Checks {
		found := false
		for _, t := range tunnels {
			if t.Name == c.Tunnel {
				found = healthCheckUsesForwardedPort(c, t)
				break
			}
		}
		if !found {
			return fmt.Errorf("update health check for %q must use a loopback entry port forwarded by that installed tunnel on this server", c.Tunnel)
		}
	}
	return nil
}

func healthCheckUsesForwardedPort(c updatehealth.Check, t Tunnel) bool {
	// Reverse clients and Direct origins have no local entry listener. The
	// same port number on such a machine could belong to an unrelated service.
	if strings.HasPrefix(t.Transport, "direct/") {
		if t.Role != "iran" {
			return false
		}
	} else if !strings.HasPrefix(t.Transport, "l3/") && t.Transport != "awg" && t.Role != "server" {
		return false
	}
	u, err := updatehealth.Endpoint(c.URL)
	if err != nil {
		return false
	}
	wantIP := net.ParseIP(u.Hostname())
	mappings, err := portmap.Expand(t.Ports, "127.0.0.1")
	if err != nil {
		return false
	}
	for _, m := range mappings {
		host, port, err := net.SplitHostPort(m.Listen)
		if err != nil || port != u.Port() {
			continue
		}
		ip := net.ParseIP(host)
		if host == "" || (ip != nil && (ip.IsUnspecified() || ip.Equal(wantIP))) {
			return true
		}
	}
	return false
}

func reportUpdateHealthFailures(bad []updatehealth.Failure, logf func(string)) string {
	names := make([]string, 0, len(bad))
	for _, f := range bad {
		names = append(names, f.Tunnel)
		logf(f.Tunnel + ": " + f.Detail)
	}
	return strings.Join(names, ", ")
}

// Both update entry points use the same decision and restore operation. The
// injected operations let tests hold systemd active while cutting a real data
// path, and verify the rollback without replacing the developer's own binary.
type updateHealthOps struct {
	services func([]string) []string
	restore  func(backup.Snapshot, func(string)) error
}

func finishUpdateHealth(snap backup.Snapshot, p updatehealth.Policy, later []string, logf func(string)) error {
	return finishUpdateHealthWith(snap, p, later, logf, updateHealthOps{
		services: unhealthyAfterUpdate,
		restore:  backup.RestoreSnapshot,
	})
}

func finishUpdateHealthWith(snap backup.Snapshot, p updatehealth.Policy, later []string, logf func(string), ops updateHealthOps) error {
	logf("Checking service health...")
	bad := ops.services(later)
	if len(bad) == 0 && len(p.Checks) != 0 {
		logf("Verifying forwarded payloads after restart...")
		ctx, cancel := context.WithTimeout(context.Background(), p.Grace())
		failures := updatehealth.Verify(ctx, p.Checks, true)
		cancel()
		if len(failures) != 0 {
			reportUpdateHealthFailures(failures, logf)
			for _, f := range failures {
				bad = append(bad, f.Tunnel+" (payload verification)")
			}
		} else {
			logf(fmt.Sprintf("Forwarded payload verified for %d tunnel(s).", len(p.Checks)))
		}
	}
	if len(bad) == 0 {
		logf("Service health checks passed.")
		return nil
	}
	why := strings.Join(bad, ", ")
	logf("Health check FAILED for: " + why)
	logf("Rolling back to the previous version...")
	if err := ops.restore(snap, logf); err != nil {
		return fmt.Errorf("update failed AND rollback failed: %s (rollback: %w) — restore manually from %s", why, err, snap.Dir)
	}
	return fmt.Errorf("update failed health check (%s) — rolled back to %s", why, snap.Meta.Version)
}
