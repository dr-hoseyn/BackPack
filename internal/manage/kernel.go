package manage

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/app"
	"github.com/backpack/backpack/internal/manage/tunnelspec"
	"github.com/backpack/backpack/internal/tui"
	"github.com/backpack/backpack/internal/tunnel/kernel"
)

func kernelBody(input config.KernelConfig) (string, error) {
	c, err := kernel.Resolve(input)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	err = toml.NewEncoder(&b).Encode(struct {
		Kernel config.KernelConfig `toml:"kernel"`
	}{c})
	return b.String(), err
}

func kernelLink(name string, c config.KernelConfig) (string, error) {
	c, err := kernel.Resolve(c)
	if err != nil {
		return "", err
	}
	peer := kernel.Peer(c)
	raw, err := json.Marshal(peer)
	if err != nil {
		return "", err
	}
	return (ShareLink{Kind: "kernel", From: c.Role, Name: name, Tr: c.Method, Tok: c.Token, Host: c.LocalAddr, Port: strconv.Itoa(c.Port), KernelPeer: string(raw)}).Encode()
}

func kernelFromLink(l ShareLink) (config.KernelConfig, error) {
	var c config.KernelConfig
	if l.Kind != "kernel" || (l.From != "iran" && l.From != "kharej") || len(l.KernelPeer) > 16<<10 {
		return c, fmt.Errorf("invalid kernel setup link")
	}
	d := json.NewDecoder(strings.NewReader(l.KernelPeer))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid kernel peer configuration")
	}
	if d.Decode(new(any)) != io.EOF {
		return c, fmt.Errorf("unexpected trailing kernel configuration")
	}
	if c.Token != l.Tok || c.Method != l.Tr || c.Role != l.PeerSide() || c.RemoteAddr != l.Host || strconv.Itoa(c.Port) != l.Port || len(c.Ports) > 0 {
		return c, fmt.Errorf("kernel setup metadata does not match the peer configuration")
	}
	return kernel.Resolve(c)
}

func installKernel(name string, input config.KernelConfig) (LinkApplied, error) {
	if runtime.GOOS != "linux" {
		return LinkApplied{}, fmt.Errorf("kernel tunnels require Linux")
	}
	if !validName(name) {
		return LinkApplied{}, fmt.Errorf("invalid tunnel name")
	}
	c, err := kernel.Resolve(input)
	if err != nil {
		return LinkApplied{}, err
	}
	body, err := kernelBody(c)
	if err != nil {
		return LinkApplied{}, err
	}
	if fileExists(app.ConfigPath(name)) {
		return LinkApplied{}, fmt.Errorf("tunnel %q already exists", name)
	}
	for _, t := range List() {
		v, e := LoadTunnelConfig(t.Name)
		if e != nil || !v.Kernel.Enabled() {
			continue
		}
		other, e := kernel.Resolve(v.Kernel)
		if e != nil {
			continue
		}
		if other.Interface == c.Interface || other.LocalIP == c.LocalIP {
			return LinkApplied{}, fmt.Errorf("kernel interface or local address already belongs to tunnel %q", t.Name)
		}
		if other.Port > 0 && c.Port == other.Port && other.LocalAddr == c.LocalAddr && other.Reverse == c.Reverse {
			return LinkApplied{}, fmt.Errorf("kernel port already belongs to tunnel %q", t.Name)
		}
		if c.LocalAddr == other.LocalAddr && c.RemoteAddr == other.RemoteAddr && (c.Method == "xfrm-ipsec" || other.Method == "xfrm-ipsec") {
			return LinkApplied{}, fmt.Errorf("IPsec needs an exclusive underlay address pair")
		}
	}
	if err = os.MkdirAll(app.ConfigDir, 0755); err != nil {
		return LinkApplied{}, err
	}
	if err = app.WriteFileAtomic(app.ConfigPath(name), []byte(body), app.TunnelConfigMode); err != nil {
		return LinkApplied{}, err
	}
	if err = writeUnit(name); err != nil {
		return LinkApplied{}, err
	}
	if err = DaemonReload(); err != nil {
		return LinkApplied{}, err
	}
	if err = StartService(app.ServiceName(name)); err != nil {
		return LinkApplied{}, err
	}
	return LinkApplied{Name: name, Kind: "kernel", Transport: c.Method, Dials: c.RemoteAddr, Service: app.ServiceName(name), Active: tunnelspec.WaitStartup(name, []byte(body))}, nil
}

func applyKernelLink(l ShareLink, o LinkApplyOptions) (LinkApplied, error) {
	c, err := kernelFromLink(l)
	if err != nil {
		return LinkApplied{}, err
	}
	name := peerName(l)
	if o.Name != "" {
		name = o.Name
	}
	return installKernel(name, c)
}

func setupKernel(side directSide) {
	tui.Clear()
	tui.Title("Standard Kernel L3 / L2 — " + sideName(side))
	defer tui.PressEnter()
	if side == sideKharej {
		l, err := DecodeShareLink(FindSetupLink(tui.Prompt("Kernel Setup Link From Iran: ")))
		if err != nil {
			tui.Error(err.Error())
			return
		}
		c, err := kernelFromLink(l)
		if err != nil {
			tui.Error(err.Error())
			return
		}
		name := tui.PromptDefault("Tunnel Name", peerName(l))
		if tui.Confirm("Create This Kernel Tunnel", true) {
			if result, e := installKernel(name, c); e != nil {
				err = e
				tui.Error(err.Error())
			} else {
				if result.Active {
					tui.Success("Created. Verify traffic to the peer tunnel address.")
				} else {
					tui.Warn("Created, but local startup is not ready. Inspect the tunnel log.")
				}
			}
		}
		return
	}
	methods := kernel.Methods()
	options := make([]tui.Option, len(methods))
	for i, m := range methods {
		desc := m.Family + " · unencrypted standard network tunnel"
		if m.Encrypted {
			desc = m.Family + " · native authenticated encryption"
		}
		options[i] = tui.Option{Title: m.Name, Desc: desc}
	}
	selected := tui.ChooseOpt("Method", options)
	if selected < 0 {
		return
	}
	name := uniqueName(tui.PromptDefault("Tunnel Name", "kernel-"+methods[selected].Name))
	c := config.KernelConfig{Method: methods[selected].Name, Role: "iran", Token: randomToken(48)}
	c.LocalAddr = strings.Trim(tui.PromptDefault("Iran Literal Underlay IP", PublicIPv4()), "[]")
	c.RemoteAddr = strings.Trim(tui.Prompt("Kharej Literal Underlay IP: "), "[]")
	c.LocalIP = tui.PromptDefault("Iran Tunnel Address With Prefix", "10.77.1.1/30")
	c.PeerIP = tui.PromptDefault("Kharej Tunnel Address", "10.77.1.2")
	c.Reverse = tui.Confirm("Kharej Initiates The Connection (Reverse)", false)
	if c.Reverse {
		tui.Info("Kernel methods use eight Noise-authenticated TCP lanes. WireGuard keeps its native UDP handshake.")
	}
	c.Port = tui.PromptInt("Carrier Port (0 = Method Default)", 0)
	c.MTU = tui.PromptInt("MTU", 1380)
	if c.Method == "gre" || c.Method == "ip6gre" || c.Method == "ecmp-lanes" {
		c.Lanes = tui.PromptInt("Parallel GRE Lanes (1..8)", 1)
		if c.Method == "ecmp-lanes" && c.Lanes == 1 {
			c.Lanes = 4
		}
	}
	if !methods[selected].IPv6Only {
		c.Ports = parsePorts(tui.Prompt("Forwarded Ports (Optional): "))
		if len(c.Ports) > 0 {
			c.AcceptUDP = tui.Confirm("Also Forward UDP", false)
		}
	}
	resolved, err := kernel.Resolve(c)
	if err != nil {
		tui.Error(err.Error())
		return
	}
	link, err := kernelLink(name, resolved)
	if err != nil {
		tui.Error(err.Error())
		return
	}
	fmt.Printf("\nMethod: %s · interface %s · peer %s\nSetup link for Kharej:\n%s\n\n", resolved.Method, resolved.Interface, resolved.PeerIP, link)
	if resolved.Method == "xfrm-ipsec" {
		tui.Info("Requires a running strongSwan charon daemon with VICI. IKEv2 negotiates fresh AES-GCM keys.")
	}
	if tui.Confirm("Create This Kernel Tunnel", true) {
		if result, e := installKernel(name, resolved); e != nil {
			err = e
			tui.Error(err.Error())
		} else {
			if result.Active {
				tui.Success("Created. Apply the setup link on Kharej, then verify traffic.")
			} else {
				tui.Warn("Created, but local startup is not ready. Inspect the tunnel log.")
			}
		}
	}
}

func editKernel(t Tunnel) {
	cfg, err := LoadTunnelConfig(t.Name)
	if err != nil {
		tui.Error(err.Error())
		tui.PressEnter()
		return
	}
	c := cfg.Kernel
	tui.Title("Kernel Tunnel — " + t.Name)
	tui.Info("Apply paired port and MTU changes on the other server too.")
	c.MTU = tui.PromptInt("MTU", c.MTU)
	if c.Port > 0 {
		c.Port = tui.PromptInt("Carrier Port", c.Port)
	}
	if c.Role == "iran" {
		c.Ports = parsePorts(tui.PromptDefault("Forwarded Ports", strings.Join(c.Ports, ", ")))
		if len(c.Ports) > 0 {
			c.AcceptUDP = tui.Confirm("Also Forward UDP", c.AcceptUDP)
		}
	}
	body, err := kernelBody(c)
	if err != nil {
		tui.Error(err.Error())
		tui.PressEnter()
		return
	}
	applyEdit(t, body)
}
