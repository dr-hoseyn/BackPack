package manage

import (
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/amneziawg"
	"github.com/backpack/backpack/internal/app"
	"github.com/backpack/backpack/internal/tui"
	"github.com/backpack/backpack/internal/tunnel/l3"
)

func awgBody(c config.AWGConfig) (string, error) {
	if err := amneziawg.Config(c).Validate(); err != nil {
		return "", err
	}
	if _, err := l3.NewForwarder(l3.Config{PeerIP: hostOf(c.PeerAddress), Ports: c.Ports, AcceptUDP: c.AcceptUDP, MaxConnections: c.MaxConnections, BandwidthMbps: c.BandwidthMbps}, nil); err != nil {
		return "", err
	}
	var b strings.Builder
	err := toml.NewEncoder(&b).Encode(struct {
		AWG config.AWGConfig `toml:"awg"`
	}{c})
	return b.String(), err
}

// The creator retains the peer's generated setup identity in its private TOML,
// just as `awg init` retains client.json. Only that identity is handed over;
// the listening machine's own private key never travels in a setup link.
func awgPeer(c config.AWGConfig, host string) (config.AWGConfig, error) {
	if c.Endpoint != "" || c.PeerSetupKey == "" {
		return config.AWGConfig{}, fmt.Errorf("only the pair creator can regenerate its peer link; import the original peer configuration on the other machine")
	}
	if net.ParseIP(host) == nil {
		return config.AWGConfig{}, fmt.Errorf("AmneziaWG setup needs the listener's literal public IP")
	}
	raw, err := base64.StdEncoding.DecodeString(c.PrivateKey)
	if err != nil {
		return config.AWGConfig{}, fmt.Errorf("invalid local identity")
	}
	key, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return config.AWGConfig{}, err
	}
	peer := c
	peer.PrivateKey, peer.PeerPublicKey, peer.PeerSetupKey = c.PeerSetupKey, base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()), ""
	peer.Address, peer.PeerAddress = c.PeerAddress, c.Address
	peer.Endpoint = net.JoinHostPort(host, strconv.Itoa(c.ListenPort))
	peer.ListenPort, peer.Keepalive = 0, 20
	peer.Ports, peer.AcceptUDP, peer.MaxConnections, peer.BandwidthMbps = nil, false, 0, 0
	return peer, amneziawg.Config(peer).Validate()
}

func awgLink(name, host string, c config.AWGConfig) (string, error) {
	peer, err := awgPeer(c, host)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(peer)
	if err != nil {
		return "", err
	}
	return (ShareLink{Kind: "awg", From: "iran", Name: name, Tr: "awg", Tok: c.PresharedKey, Host: host, Port: strconv.Itoa(c.ListenPort), AWGPeer: string(body)}).Encode()
}

func awgFromLink(l ShareLink) (config.AWGConfig, error) {
	if l.Kind != "awg" || l.From != "iran" || l.Tr != "awg" || len(l.AWGPeer) > 16<<10 {
		return config.AWGConfig{}, fmt.Errorf("invalid AmneziaWG setup link")
	}
	var c config.AWGConfig
	d := json.NewDecoder(strings.NewReader(l.AWGPeer))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid AmneziaWG peer configuration")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return config.AWGConfig{}, fmt.Errorf("unexpected trailing AmneziaWG configuration")
	}
	if c.PresharedKey != l.Tok || c.Endpoint != net.JoinHostPort(l.Host, l.Port) || c.ListenPort != 0 || c.PeerSetupKey != "" || len(c.Ports) != 0 {
		return config.AWGConfig{}, fmt.Errorf("AmneziaWG link metadata does not match its peer identity")
	}
	return c, amneziawg.Config(c).Validate()
}

func installAWG(name string, c config.AWGConfig) (LinkApplied, error) {
	if !validName(name) {
		return LinkApplied{}, fmt.Errorf("invalid tunnel name")
	}
	body, err := awgBody(c)
	if err != nil {
		return LinkApplied{}, err
	}
	if fileExists(app.ConfigPath(name)) {
		return LinkApplied{}, fmt.Errorf("tunnel %q already exists", name)
	}
	for _, t := range List() {
		v, err := LoadTunnelConfig(t.Name)
		if err == nil && v.AWG.Enabled() && (v.AWG.Interface == c.Interface || v.AWG.PrivateKey == c.PrivateKey) {
			return LinkApplied{}, fmt.Errorf("AmneziaWG identity or interface already belongs to tunnel %q", t.Name)
		}
	}
	if err := os.MkdirAll(app.ConfigDir, 0755); err != nil {
		return LinkApplied{}, err
	}
	if err := app.WriteFileAtomic(app.ConfigPath(name), []byte(body), app.TunnelConfigMode); err != nil {
		return LinkApplied{}, err
	}
	if err := writeUnit(name); err != nil {
		return LinkApplied{}, err
	}
	if err := DaemonReload(); err != nil {
		return LinkApplied{}, err
	}
	if err := StartService(app.ServiceName(name)); err != nil {
		return LinkApplied{}, err
	}
	return LinkApplied{Name: name, Kind: "awg", Dials: c.Endpoint, Transport: "awg", Service: app.ServiceName(name), Active: WaitServiceActive(app.ServiceName(name), 10*time.Second)}, nil
}

func applyAWGLink(l ShareLink, o LinkApplyOptions) (LinkApplied, error) {
	c, err := awgFromLink(l)
	if err != nil {
		return LinkApplied{}, err
	}
	name := peerName(l)
	if o.Name != "" {
		name = o.Name
	}
	return installAWG(name, c)
}

func setupAWG(side directSide) {
	tui.Clear()
	tui.Title("AmneziaWG — " + sideName(side))
	if side == sideKharej {
		raw := FindSetupLink(tui.Prompt("AmneziaWG Setup Link From Iran: "))
		l, err := DecodeShareLink(raw)
		if err != nil {
			tui.Error(err.Error())
			tui.PressEnter()
			return
		}
		c, err := awgFromLink(l)
		if err != nil {
			tui.Error(err.Error())
			tui.PressEnter()
			return
		}
		name := tui.PromptDefault("Tunnel Name", peerName(l))
		c.Interface = tui.PromptDefault("Exclusive Interface Name", c.Interface)
		tui.Info("Peer: " + c.Endpoint + " · local " + c.Address + " · peer " + c.PeerAddress)
		if tui.Confirm("Create This AmneziaWG Tunnel", true) {
			_, err = installAWG(name, c)
			if err != nil {
				tui.Error(err.Error())
			} else {
				tui.Success("Installed; verify traffic after the handshake.")
			}
		}
		tui.PressEnter()
		return
	}
	host := strings.Trim(strings.TrimSpace(tui.PromptDefault("Iran Literal Public IP", PublicIPv4())), "[]")
	port := tui.PromptDefault("Public UDP Port", "25999")
	name := uniqueName(tui.PromptDefault("Tunnel Name", "awg-iran-"+port))
	local := tui.PromptDefault("Iran Tunnel Host Prefix", "10.254.248.1/32")
	peer := tui.PromptDefault("Kharej Tunnel Host Prefix", "10.254.248.2/32")
	a, b, err := amneziawg.NewPair(net.JoinHostPort(host, port), local, peer)
	if err != nil {
		tui.Error(err.Error())
		tui.PressEnter()
		return
	}
	c := config.AWGConfig(a)
	c.PeerSetupKey = b.PrivateKey
	sum := sha256.Sum256([]byte(name))
	c.Interface = tui.PromptDefault("Exclusive Interface Name", fmt.Sprintf("bpawg%x", sum[:4]))
	c.Ports = parsePorts(tui.Prompt("Forwarded Ports (Optional; Bare Targets Reach Kharej's Tunnel IP): "))
	if len(c.Ports) > 0 {
		c.AcceptUDP = tui.Confirm("Also Carry UDP On Forwarded Ports", false)
	}
	link, err := awgLink(name, host, c)
	if err != nil {
		tui.Error(err.Error())
		tui.PressEnter()
		return
	}
	if _, err := awgBody(c); err != nil {
		tui.Error(err.Error())
		tui.PressEnter()
		return
	}
	tui.Info("UDP " + port + " · " + c.Address + " ↔ " + c.PeerAddress + " · " + c.Interface)
	tui.Info("Only the peer host route is installed. A backend must listen on its tunnel IP or wildcard.")
	if tui.Confirm("Create This AmneziaWG Tunnel", true) {
		if _, err := installAWG(name, c); err != nil {
			tui.Error(err.Error())
		} else {
			printLinkBlock(link, "Setup Kharej → AmneziaWG")
			tui.Success("Installed; the peer link contains private credentials.")
		}
	}
	tui.PressEnter()
}

func editAWG(t Tunnel) {
	cfg, err := LoadTunnelConfig(t.Name)
	if err != nil {
		tui.Error(err.Error())
		tui.PressEnter()
		return
	}
	c := cfg.AWG
	tui.Info("AmneziaWG · " + c.Interface + " · " + c.Address + " ↔ " + c.PeerAddress)
	c.Ports = parsePorts(tui.PromptDefault("Forwarded Ports (Targets Must Be Reachable Through The Peer Host Route)", strings.Join(c.Ports, ", ")))
	c.AcceptUDP = tui.Confirm("Also Carry UDP", c.AcceptUDP)
	body, err := awgBody(c)
	if err != nil {
		tui.Error(err.Error())
		tui.PressEnter()
		return
	}
	if tui.Confirm("Save And Restart This Tunnel", true) {
		applyEdit(t, body)
	}
}

func applyAWGEdit(c config.AWGConfig, e DirectEdit) (config.AWGConfig, error) {
	if e.MTU != nil || e.AutoMTU != nil || e.Paths != nil || e.FEC != nil || e.Spoof != nil || e.Stealth != nil || e.Preset != nil || e.Sessions != nil || e.MSS != nil || e.Addr != nil {
		return c, fmt.Errorf("AmneziaWG edits support forwarded ports, UDP and forwarding limits; paired identity and MTU changes require matching peer configuration")
	}
	if e.Ports != nil {
		c.Ports = parsePorts(*e.Ports)
	}
	if e.AcceptUDP != nil {
		c.AcceptUDP = *e.AcceptUDP
	}
	if e.MaxConnections != nil {
		c.MaxConnections = *e.MaxConnections
	}
	if e.BandwidthMbps != nil {
		c.BandwidthMbps = *e.BandwidthMbps
	}
	_, err := awgBody(c)
	return c, err
}
