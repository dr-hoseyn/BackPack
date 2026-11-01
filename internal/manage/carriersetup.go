package manage

import (
	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tui"
)

func askCarrier(s *TunnelSpec) bool {
	switch s.Transport {
	case "tls", "grpc":
		if s.Role == "client" {
			s.CarrierOptions.ServerName = tui.PromptDefault("TLS Server Name (SNI)", s.CarrierOptions.ServerName)
			s.CarrierOptions.Fingerprint = tui.PromptDefault("Browser (chrome/firefox/safari/edge/random)", orDefault(s.CarrierOptions.Fingerprint, "chrome"))
			s.CarrierOptions.TLSFragment = tui.Confirm("Split The TLS ClientHello (Experimental)", s.CarrierOptions.TLSFragment)
		}
		if s.Transport == "grpc" {
			s.CarrierOptions.GRPCService = tui.PromptDefault("gRPC Service (Same On Both Ends)", orDefault(s.CarrierOptions.GRPCService, "Backpack"))
		}
	case "ssh":
		if s.Role == "server" {
			tui.Info("SSH Forwards To This Tunnel's Loopback Relay. AllowTcpForwarding Must Be Enabled.")
			break
		}
		o := &s.CarrierOptions
		port := o.SSHPort
		if port == 0 {
			port = 22
		}
		o.SSHPort = tui.PromptInt("Server SSH Port", port)
		o.SSHUser = tui.PromptDefault("SSH User", orDefault(o.SSHUser, "root"))
		o.SSHHostKey = tui.PromptDefault("Server SSH Host Fingerprint (SHA256:...)", o.SSHHostKey)
		o.SSHKeyFile = tui.Prompt("SSH Private Key File (Blank Uses Password): ")
		if o.SSHKeyFile == "" {
			o.SSHPassword = tui.Prompt("SSH Password: ")
		}
	}
	if err := s.CarrierOptions.Validate(config.TransportType(s.Transport), s.Role == "client"); err != nil {
		tui.Error(err.Error())
		tui.PressEnter()
		return false
	}
	return true
}

func askCarrierForm(f *PeerForm) bool {
	role := "client"
	if f.Side == "iran" {
		role = "server"
	}
	s := TunnelSpec{Role: role, Transport: f.Transport, CarrierOptions: f.CarrierOptions}
	if !askCarrier(&s) {
		return false
	}
	f.CarrierOptions = s.CarrierOptions
	return true
}
