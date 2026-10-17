package manage

import (
	"crypto/sha256"
	"fmt"
	"strconv"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tui"
)

func configureManagedPaths(n *TunnelSpec, old TunnelSpec, public, host string, probe realitySetupProbe) bool {
	count := len(old.ServerPaths)
	if n.Role == "client" {
		count = len(old.ClientPaths)
	}
	if count < 2 {
		count = 2
	}
	count = tui.PromptInt("Number Of Independent Managed Paths (2..8)", count)
	if count < 2 || count > 8 {
		tui.Error("Choose 2..8 paths.")
		return false
	}
	var options []tui.Option
	var protocols []string
	for _, g := range cliTransportGroups() {
		for _, entry := range g.entries {
			if managedTransport(entry.value) && entry.value != "managed-paths" {
				options = append(options, tui.Option{Title: entry.label, Desc: entry.desc})
				protocols = append(protocols, entry.value)
			}
		}
	}
	n.AcceptUDP = false
	for i := range count {
		previousName, previousEndpoint, previousExport := "path"+strconv.Itoa(i+1), public, ""
		one := *n
		clearManagedTransport(&one)
		if n.Role == "server" && i < len(old.ServerPaths) {
			p := old.ServerPaths[i]
			previousName, previousEndpoint, previousExport = p.Name, pathServerEndpoint(p), p.Endpoint
			one.NaiveServer, one.XrayServer, one.SingServer, one.ShortServer = p.Naive, p.Xray, p.Sing, p.Short
		} else if n.Role == "client" && i < len(old.ClientPaths) {
			p := old.ClientPaths[i]
			previousName, previousEndpoint = p.Name, pathClientEndpoint(p)
			one.NaiveClient, one.XrayClient, one.SingClient, one.ShortClient = p.Naive, p.Xray, p.Sing, p.Short
		}
		name := tui.PromptDefault("Path Identity (Same On Both Machines)", previousName)
		choice := tui.ChooseOpt(fmt.Sprintf("Carrier For %s", name), options)
		if choice < 0 || choice >= len(protocols) {
			return false
		}
		endpoint := tui.PromptDefault("Public Listener On Iran / Public Endpoint On Kharej (Host:Port)", previousEndpoint)
		one.Name = managedPathChildName(one.Name, i+1)
		if !setupManagedCarrierWithProbe(&one, protocols[choice], endpoint, host, probe) {
			return false
		}
		if one.BindAddr != n.BindAddr || one.RemoteAddr != n.RemoteAddr {
			tui.Error("Every path must use the same private Reverse target.")
			return false
		}
		if n.Role == "server" {
			export := tui.PromptDefault("Public Host:Port For Pairing (Empty Uses Shared Public Host)", previousExport)
			n.ServerPaths = append(n.ServerPaths, config.ManagedServerPath{Name: name, Endpoint: export, Naive: one.NaiveServer, Xray: one.XrayServer, Sing: one.SingServer, Short: one.ShortServer})
			n.AcceptUDP = n.AcceptUDP || one.AcceptUDP
		} else {
			n.ClientPaths = append(n.ClientPaths, config.ManagedClientPath{Name: name, Naive: one.NaiveClient, Xray: one.XrayClient, Sing: one.SingClient, Short: one.ShortClient})
		}
	}
	if n.Role == "client" {
		n.PathPolicy = old.PathPolicy
		n.ConnectionPool = 8
		for _, p := range n.ClientPaths {
			if p.Short.Enabled() {
				n.AggressivePool = false
			}
		}
		if tui.Confirm("Tune Verified Path Probing And Failover", false) {
			p := &n.PathPolicy
			p.ProbeInterval = tui.PromptInt("Probe Interval Seconds (0 = 5)", p.ProbeInterval)
			p.ProbeTimeout = tui.PromptInt("Probe Timeout Seconds (0 = 5)", p.ProbeTimeout)
			p.ProbeBytes = tui.PromptInt("Verified Bytes Per Direction (0 = 16384)", p.ProbeBytes)
			p.Failures = tui.PromptInt("Failures Before Withdrawal (0 = 2)", p.Failures)
			p.Recoveries = tui.PromptInt("Successes Before Recovery (0 = 2)", p.Recoveries)
			p.MinDwell = tui.PromptInt("Minimum Healthy Dwell Seconds (0 = 30)", p.MinDwell)
			p.Improvement = tui.PromptInt("Required Improvement Percent (0 = 20)", p.Improvement)
		}
	}
	return true
}

func managedPathChildName(parent string, index int) string {
	if len(parent) > 30 {
		// Keep long tunnel names distinct without exceeding the certificate
		// helper's name budget. Truncation alone could overwrite another
		// tunnel's generated private TLS material.
		sum := sha256.Sum256([]byte(parent))
		parent = fmt.Sprintf("%s-%x", parent[:15], sum[:6])
	}
	return parent + "-p" + strconv.Itoa(index)
}
