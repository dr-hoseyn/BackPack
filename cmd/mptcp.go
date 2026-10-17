package cmd

import (
	"fmt"
	"runtime"

	"github.com/backpack/backpack/config"
)

func checkMPTCP(c *config.Config) error {
	for _, side := range []struct {
		on        bool
		transport config.TransportType
		managed   bool
		proxy     string
	}{
		{c.Server.MPTCP, c.Server.Transport, c.Server.Managed(), ""},
		{c.Client.MPTCP, c.Client.Transport, c.Client.Managed(), c.Client.Proxy},
	} {
		if !side.on {
			continue
		}
		if runtime.GOOS != "linux" || side.managed || side.proxy != "" {
			return fmt.Errorf("mptcp requires native Linux TCP tunnel sockets without managed helpers or an outbound proxy")
		}
		if side.transport != config.TCP && side.transport != config.TCPMUX && side.transport != config.STEALTH {
			return fmt.Errorf("mptcp applies only to tcp, stealth and tcpmux")
		}
	}
	return nil
}
