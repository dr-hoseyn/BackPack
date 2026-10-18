package manage

import (
	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tui"
)

func configureConnect(n *TunnelSpec, old TunnelSpec, protocol, public, host string) bool {
	mode := "h3"
	if protocol == "trusttunnel" {
		mode = "auto"
	}
	if n.Role == "client" {
		x := old.ConnectClient
		if x.Protocol != protocol {
			x = config.ConnectClientConfig{}
		}
		x.Protocol, x.Server = protocol, public
		x.ServerName = tui.PromptDefault("Verified TLS Name From Iran", defaultString(x.ServerName, host))
		x.Username = tui.PromptDefault("Carrier Username From Iran", defaultString(x.Username, "backpack"))
		x.Password = managedSecret("Carrier Password From Iran", x.Password, "")
		x.CAFile = tui.PromptDefault("Private CA File (Empty Uses System Roots)", x.CAFile)
		if protocol == "trusttunnel" {
			mode = tui.PromptDefault("Transport (auto/h2/h3)", defaultString(x.Mode, mode))
		} else {
			x.Path = tui.PromptDefault("Private Session Path From Iran", x.Path)
		}
		x.Mode = mode
		n.ConnectClient = x
		return true
	}
	x := old.ConnectServer
	if x.Protocol != protocol {
		x = config.ConnectServerConfig{}
	}
	x.Protocol, x.Listen = protocol, public
	x.ServerName = tui.PromptDefault("Verified TLS Name", defaultString(x.ServerName, host))
	x.Username = tui.PromptDefault("Carrier Username", defaultString(x.Username, "backpack"))
	x.Password = managedSecret("Separate Carrier Password", x.Password, randomToken(32))
	if protocol == "trusttunnel" {
		mode = tui.PromptDefault("Transport (auto/h2/h3)", defaultString(x.Mode, mode))
		tui.Info("auto reserves TCP and UDP on the same port; verified HTTP/3 falls back to HTTP/2.")
	} else {
		x.Path = tui.PromptDefault("Private Session Path", defaultString(x.Path, "/"+randomToken(24)))
	}
	x.Mode = mode
	var ok bool
	x.Certificate, x.Key, ok = managedCertificate(*n, x.ServerName, x.Certificate, x.Key)
	if !ok {
		return false
	}
	n.ConnectServer = x
	return true
}
