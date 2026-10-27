package cmd

import (
	"runtime"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/manage/host"
	"github.com/backpack/backpack/internal/optimize"
)

var enableThroughputCapacity = optimize.EnableHighConcurrency

// Throughput opts into the host capacity policy as well as transport windows.
// Read the saved preset at each generation so existing tunnels gain the policy
// after an update and a live preset change takes effect on reload. Do not
// re-expand presets: manually adjusted transport values must survive updates.
func applyThroughputHostTuning(cfg *config.Config) {
	if runtime.GOOS != "linux" {
		return
	}
	selected, addr, ports := throughputHostPolicy(cfg)
	if !selected {
		return
	}
	reserve := append(host.ReservedPorts(), host.ReservedPortsFor(addr, ports)...)
	for _, binding := range portsInUse(cfg) {
		reserve = append(reserve, host.ReservedPortsFor(binding.address, nil)...)
	}
	if _, err := enableThroughputCapacity(reserve); err != nil {
		logger.Warnf("Throughput host capacity could not be applied: %v — continuing with current host settings", err)
		return
	}
	logger.Info("Throughput host capacity applied: reserved service ports, expanded ephemeral range and disabled supported busy polling")
}

func throughputHostPolicy(cfg *config.Config) (selected bool, addr string, ports []string) {
	switch {
	case cfg.AWG.Enabled():
		return false, "", nil
	case cfg.L3.Enabled():
		return cfg.L3.Preset == "throughput", cfg.L3.Addr, cfg.L3.Ports
	case cfg.Direct.Enabled():
		return cfg.Direct.Preset == "throughput", cfg.Direct.Addr, cfg.Direct.Ports
	case cfg.Server.BindAddr != "":
		return cfg.Server.Preset == "throughput" && !cfg.Server.SkipOptz, cfg.Server.BindAddr, cfg.Server.Ports
	case cfg.Client.RemoteAddr != "":
		return cfg.Client.Preset == "throughput" && !cfg.Client.SkipOptz, "", nil
	default:
		return false, "", nil
	}
}
