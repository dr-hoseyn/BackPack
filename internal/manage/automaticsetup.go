package manage

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tui"
)

// Automatic uses the existing verified adaptive engine. Only carriers that
// can independently reach the same private Reverse listener belong in this
// pool; native L3/Direct engines cannot be mixed into it.
func automaticCarriers(sing, xray, naive bool, cover string) []string {
	carriers := []string{"short-https"}
	if sing {
		if naive {
			carriers = append(carriers, "naive")
		}
		carriers = append(carriers, "hysteria2", "anytls", "tuic")
	}
	if xray {
		carriers = append(carriers, "xhttp")
		if cover != "" {
			carriers = append(carriers, "reality")
		}
	}
	if sing && cover != "" {
		carriers = append(carriers, "shadowtls")
	}
	return carriers
}

func automaticExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0
}

// Hold both sockets until the plan is accepted. This avoids selecting an
// occupied UDP port for QUIC or selecting the same port twice. Service startup
// still validates binds after releasing these temporary reservations.
func reserveAutomaticPort(host string, ports []string, used map[int]bool) (string, func(), error) {
	for attempt := 0; attempt < 100; attempt++ {
		candidate := "0"
		if attempt == 0 && !used[443] && host == "0.0.0.0" {
			candidate = "443"
		}
		tcp, err := net.Listen("tcp4", net.JoinHostPort(host, candidate))
		if err != nil {
			continue
		}
		port := tcp.Addr().(*net.TCPAddr).Port
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		// Compare against requested forwards without treating our own
		// temporary listener as a forwarding conflict.
		conflict := automaticForwardPort(ports, port)
		if used[port] || conflict {
			tcp.Close()
			continue
		}
		udp, err := net.ListenPacket("udp4", addr)
		if err != nil {
			tcp.Close()
			continue
		}
		used[port] = true
		return addr, func() { tcp.Close(); udp.Close() }, nil
	}
	return "", nil, fmt.Errorf("could not reserve an independent tunnel port")
}

func automaticForwardPort(ports []string, port int) bool {
	for _, spec := range ports {
		local, _, _ := strings.Cut(spec, "=")
		if _, p, err := net.SplitHostPort(local); err == nil {
			local = p
		}
		lo, hi, ranged := strings.Cut(local, "-")
		a, _ := strconv.Atoi(strings.TrimSpace(lo))
		b := a
		if ranged {
			b, _ = strconv.Atoi(strings.TrimSpace(hi))
		}
		if port >= a && port <= b {
			return true
		}
	}
	return false
}

func automaticUDPConflict(ports []string) string {
	for _, spec := range ports {
		local, _, _ := strings.Cut(spec, "=")
		host := ""
		if h, p, err := net.SplitHostPort(local); err == nil {
			host, local = h, p
		}
		lo, hi, ranged := strings.Cut(local, "-")
		from, _ := strconv.Atoi(lo)
		to := from
		if ranged {
			to, _ = strconv.Atoi(hi)
		}
		for p := from; p <= to; p++ {
			addr := net.JoinHostPort(host, strconv.Itoa(p))
			if TunnelPortInUse("udp", addr) {
				return addr
			}
		}
	}
	return ""
}

type automaticTLSMaterial struct {
	Cert, Key                      string
	XHTTPName, XHTTPCert, XHTTPKey string
}

func automaticCertificates(name, host string, xhttp bool) (automaticTLSMaterial, error) {
	var material automaticTLSMaterial
	var err error
	material.Cert, material.Key, err = EnsureSelfSignedCert(name+"-https", host)
	if err != nil {
		return material, err
	}
	// Xray's verified XHTTP contract requires a DNS identity. The dial address
	// can remain an IP: pair a private DNS SAN and its trust file explicitly,
	// without requiring public DNS or weakening certificate verification.
	if xhttp && net.ParseIP(host) != nil {
		material.XHTTPName = name + ".backpack.invalid"
		material.XHTTPCert, material.XHTTPKey, err = EnsureSelfSignedCert(name+"-xhttp", material.XHTTPName)
	}
	return material, err
}

func buildAutomaticSpec(name, host, private string, material automaticTLSMaterial, cover string, carriers, endpoints, ports []string) (TunnelSpec, error) {
	cert, key := material.Cert, material.Key
	s := TunnelSpec{Name: name, Role: "server", Transport: "tcp", BindAddr: private, Token: randomToken(64), Ports: ports}
	ApplyPreset(&s, PresetBalance)
	if len(carriers) < 2 || len(carriers) > 8 || len(endpoints) != len(carriers) {
		return TunnelSpec{}, fmt.Errorf("automatic setup requires 2..8 independent compatible carriers")
	}
	for i, carrier := range carriers {
		p := config.ManagedServerPath{Name: carrier, Endpoint: net.JoinHostPort(host, addrPort(endpoints[i]))}
		id, err := managedUUID()
		if err != nil {
			return TunnelSpec{}, err
		}
		switch carrier {
		case "short-https":
			p.Short = config.ShortServerConfig{Listen: endpoints[i], ServerName: host, Certificate: cert, Key: key}
		case "naive":
			p.Naive = config.NaiveServerConfig{Binary: managedHelperPath("sing-box"), Listen: endpoints[i], Username: "backpack", Password: randomToken(32), Certificate: cert, Key: key}
		case "trusttunnel", "webtransport":
			x := config.ConnectServerConfig{Protocol: carrier, Listen: endpoints[i], ServerName: host, Username: "backpack", Password: randomToken(32), Certificate: cert, Key: key, Mode: "auto"}
			if carrier == "webtransport" {
				x.Mode = "h3"
				x.Path = "/" + randomToken(24)
			}
			p.Connect = x
		case "tailcat":
			x, err := automaticTailcat(endpoints[i])
			if err != nil {
				return TunnelSpec{}, err
			}
			p.Sing = x
		case "xhttp", "reality", "xhttp-reality":
			x := config.XrayServerConfig{Binary: managedHelperPath("xray"), Listen: endpoints[i], Mode: carrier, UUID: id, ServerName: host}
			if carrier == "xhttp" {
				x.Path, x.Certificate, x.Key = "/"+randomToken(24), cert, key
				if net.ParseIP(host) != nil {
					if material.XHTTPName == "" || material.XHTTPCert == "" || material.XHTTPKey == "" {
						return TunnelSpec{}, fmt.Errorf("IP-based Automatic XHTTP requires a paired private DNS identity and certificate")
					}
					x.ServerName, x.Certificate, x.Key = material.XHTTPName, material.XHTTPCert, material.XHTTPKey
				}
			} else {
				x.Target = cover
				x.ServerName = addrHost(cover, "")
				x.PrivateKey, _, err = managedRealityKey("")
				if err != nil {
					return TunnelSpec{}, err
				}
				var shortID [8]byte
				if _, err := rand.Read(shortID[:]); err != nil {
					return TunnelSpec{}, err
				}
				x.ShortID = hex.EncodeToString(shortID[:])
				if config.XrayHTTP(carrier) {
					x.Path = "/" + randomToken(24)
				}
			}
			p.Xray = x
		case "hysteria2", "anytls", "tuic", "shadowtls":
			x := config.SingServerConfig{Binary: managedHelperPath("sing-box"), Listen: endpoints[i], Protocol: carrier, ServerName: host, Password: randomToken(32), Certificate: cert, Key: key}
			if carrier == "tuic" {
				x.UUID = id
			}
			if carrier == "hysteria2" {
				x.ObfsPassword = randomToken(32)
			}
			if config.SingDatagram(carrier) {
				x.NativeUDP = true
				s.AcceptUDP = true
			}
			if carrier == "shadowtls" {
				var secret [32]byte
				if _, err := rand.Read(secret[:]); err != nil {
					return TunnelSpec{}, err
				}
				x.ShadowsocksKey = base64.StdEncoding.EncodeToString(secret[:])
				x.Handshake, x.ServerName = cover, addrHost(cover, "")
				x.Certificate, x.Key = "", ""
			}
			p.Sing = x
		default:
			return TunnelSpec{}, fmt.Errorf("unsupported automatic carrier %q", carrier)
		}
		s.ServerPaths = append(s.ServerPaths, p)
	}
	return s, nil
}

func setupAutomaticIran() {
	tui.Clear()
	tui.Title("Automatic Tunnel — Iran")
	if runtime.GOOS != "linux" {
		tui.Error("Automatic tunnels require Linux.")
		tui.PressEnter()
		return
	}
	fail := func(err error) { tui.Error(err.Error()); tui.PressEnter() }
	editor := forwardPortEditor{defaultHost: "127.0.0.1", reverse: true}
	editor.setupHelp()
	ports, err := editor.setupPorts(tui.Prompt("Forwarded Ports (e.g. 443, 8080=127.0.0.1:2096): "))
	if err == nil && len(ports) == 0 {
		err = fmt.Errorf("at least one forwarded port is required")
	}
	if err == nil {
		err = validatePortSpecs(ports)
	}
	if err != nil {
		fail(err)
		return
	}
	if busy := reverseBusyPorts(ports, "", "tcp"); len(busy) != 0 {
		fail(fmt.Errorf("forwarded ports already in use: %s", strings.Join(busy, ", ")))
		return
	}
	host := PublicIPv4()
	if host == "" || host == "-" {
		host = strings.Trim(strings.TrimSpace(tui.Prompt("Iran Public IP Or Domain: ")), "[]")
	}
	if host == "" || strings.ContainsAny(host, "/ \t\r\n") {
		fail(fmt.Errorf("a public IP or domain is required"))
		return
	}
	sing, xray := automaticExecutable(managedHelperPath("sing-box")), automaticExecutable(managedHelperPath("xray"))
	if sing {
		if busy := automaticUDPConflict(ports); busy != "" {
			fail(fmt.Errorf("forwarded UDP port already in use: %s", busy))
			return
		}
	}
	cover := ""
	if xray {
		tui.Info("Checking verified covers for REALITY and ShadowTLS...")
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		results := scanRealityCovers(ctx, realityCoverSuggestions, managedHelperPath("xray"), probeRealitySetup)
		interrupted := ctx.Err() != nil
		cancel()
		if interrupted {
			return
		}
		for _, r := range results {
			if r.err == nil {
				cover = r.target
				break
			}
		}
	}
	carriers := automaticCarriers(sing, xray, automaticExecutable(managedHelperPath("naive")), cover)
	carriers = modernAutomaticCarriers(carriers)
	profile := "full"
	if tui.Confirm("Use Economy (Up To Three Carriers, Reduced Backup Probes)", true) {
		profile = "economy"
		carriers = economicalCarriers(carriers)
	}
	if len(carriers) < 2 {
		fail(fmt.Errorf("install Backpack's pinned sing-box or Xray helper first; Automatic needs at least two compatible carriers"))
		return
	}
	if !sing {
		tui.Warn("Sing-box is unavailable: Naive, Hy2, AnyTLS, TUIC and ShadowTLS omitted. TCP forwards only.")
	}
	if !xray {
		tui.Warn("Xray is unavailable: XHTTP and REALITY omitted.")
	}
	if cover == "" {
		tui.Warn("No authenticated cover was verified: REALITY and ShadowTLS omitted.")
	}
	used := map[int]bool{}
	for _, p := range ReservedPorts() {
		used[p] = true
	}
	var reservations []func()
	release := func() {
		for _, close := range reservations {
			close()
		}
		reservations = nil
	}
	defer release()
	reserve := func(bind string) (string, error) {
		addr, close, err := reserveAutomaticPort(bind, ports, used)
		if err == nil {
			reservations = append(reservations, close)
		}
		return addr, err
	}
	private, err := reserve("127.0.0.1")
	if err != nil {
		fail(err)
		return
	}
	var endpoints []string
	for _, carrier := range carriers {
		addr, err := reserve("0.0.0.0")
		if err != nil {
			fail(err)
			return
		}
		endpoints = append(endpoints, addr)
		network := "TCP"
		if config.SingDatagram(carrier) {
			network = "UDP"
		}
		tui.Info(fmt.Sprintf("Allow inbound %s %s — %s", network, addrPort(addr), transportLabel(carrier)))
	}
	tui.Info("Forwarded ports: " + strings.Join(ports, ", "))
	if sing {
		tui.Info("TCP + native UDP forwarding; each uses its own verified healthy paths.")
	}
	tui.Info("Iran address: " + host + ". Kharej uses one setup link; matching pinned helpers are required there.")
	if !tui.Confirm("Create Automatic Tunnel", true) {
		return
	}
	name := freeName("automatic-" + addrPort(private))
	material, err := automaticCertificates(name, host, xray)
	if err != nil {
		fail(err)
		return
	}
	s, err := buildAutomaticSpec(name, host, private, material, cover, carriers, endpoints, ports)
	s.PathProfile = profile
	if err == nil {
		err = validateManagedSpec(s)
	}
	if err != nil {
		fail(err)
		return
	}
	link := pendingReverseLink(s, host, linkExtras{})
	if link == "" {
		fail(fmt.Errorf("could not build the paired setup link"))
		return
	}
	release()
	if !finishSetup(s) {
		return
	}
	printLinkBlock(link, "sudo backpack → Setup Kharej → Setup Link")
	tui.Info("Only verified paths carry traffic. Existing streams stay on their path; new streams can fail over.")
	tui.PressEnter()
}
