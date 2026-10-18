package manage

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/carrierdiag"
	"github.com/backpack/backpack/internal/nativeudp"
	"github.com/backpack/backpack/internal/pathprobe"
	"github.com/backpack/backpack/internal/socks"
	"github.com/backpack/backpack/internal/tunnel/naive"
	"github.com/backpack/backpack/internal/utils"
	"github.com/sirupsen/logrus"
)

// DiagnoseDatagramConfig starts disposable helpers only. It never restarts the
// configured tunnel, claims its control seat, or connects to a forwarded backend.
func DiagnoseDatagramConfig(ctx context.Context, path, selected string) ([]carrierdiag.Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open the configuration file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > 1<<20 {
		return nil, errors.New("configuration must be at most 1 MiB")
	}
	var cfg config.Config
	if _, err := toml.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&cfg); err != nil {
		return nil, errors.New("cannot parse configuration; run backpack check locally for details")
	}
	if cfg.Client.RemoteAddr == "" || cfg.Server.BindAddr != "" || cfg.Direct.Enabled() || cfg.L3.Enabled() || cfg.AWG.Enabled() {
		return nil, errors.New("run this diagnostic with the Kharej Reverse client configuration")
	}
	var clients []config.ClientConfig
	var names []string
	if len(cfg.Client.Paths) > 8 {
		return nil, errors.New("at most eight managed paths are supported")
	}
	if len(cfg.Client.Paths) > 0 {
		for _, p := range cfg.Client.Paths {
			if !config.SingDatagram(p.Sing.Protocol) || (selected != "" && p.Name != selected) {
				continue
			}
			c := cfg.Client
			c.Paths = nil
			c.Sing = p.Sing
			clients = append(clients, c)
			names = append(names, p.Name)
		}
	} else if config.SingDatagram(cfg.Client.Sing.Protocol) && (selected == "" || selected == cfg.Client.Sing.Protocol) {
		clients = append(clients, cfg.Client)
		names = append(names, cfg.Client.Sing.Protocol)
	}
	if len(clients) == 0 {
		return nil, errors.New("no matching TUIC or Hysteria2 client; --path selects a configured managed path identity")
	}
	var reports []carrierdiag.Report
	for i, c := range clients {
		if ctx.Err() != nil {
			return reports, ctx.Err()
		}
		reports = append(reports, diagnoseDatagramClient(ctx, names[i], c))
	}
	return reports, nil
}

func helperDiagnosticSignal(log *os.File) (string, string) {
	// Read only a bounded tail and map explicit helper messages to fixed text.
	// Never place raw helper output, credentials or generated JSON in a report.
	info, err := log.Stat()
	if err != nil {
		return "helper_route_unverified", "The helper could not establish the fixed Reverse route; authentication is not confirmed."
	}
	start := max(int64(0), info.Size()-(64<<10))
	b := make([]byte, info.Size()-start)
	_, _ = log.ReadAt(b, start)
	s := strings.ToLower(string(b))
	for _, pattern := range []string{"authentication failed", "authentication error", "invalid password", "invalid user"} {
		if strings.Contains(s, pattern) {
			return "helper_reported_auth_failure", "The pinned helper explicitly reported an authentication failure. Check paired credentials on both machines."
		}
	}
	for _, pattern := range []string{"certificate signed by unknown authority", "certificate is not valid for", "certificate has expired"} {
		if strings.Contains(s, pattern) {
			return "helper_reported_certificate_failure", "The actual helper reported a certificate verification failure. Check the paired CA, TLS name and certificate validity."
		}
	}
	if strings.Contains(s, "timeout: no recent network activity") {
		return "helper_reported_quic_timeout", "The actual helper reported a QUIC activity timeout. This alone does not identify network filtering or reject the credentials."
	}
	return "helper_route_unverified", "The helper could not establish the fixed Reverse route; authentication is not confirmed."
}

func diagnoseDatagramClient(parent context.Context, name string, c config.ClientConfig) carrierdiag.Report {
	r := carrierdiag.Report{Path: name, Protocol: c.Sing.Protocol}
	add := func(name, status, code, detail string) {
		r.Stages = append(r.Stages, carrierdiag.Stage{Name: name, Status: status, Code: code, Detail: detail})
	}
	if err := naive.ValidateSingClient(&c); err != nil {
		add("config", "fail", "invalid_local_configuration", "Local helper settings, credentials, private target or certificate trust are invalid. Run backpack check for local details.")
		return r
	}
	add("config", "pass", "valid_local_configuration", "Local carrier configuration passed validation; this does not verify the remote credentials.")
	r.Stages = append(r.Stages, carrierdiag.NetworkStages(parent, c.Sing)...)
	if parent.Err() != nil {
		return r
	}
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	output, err := os.CreateTemp("", "backpack-diagnose-*.log")
	if err != nil {
		add("helper", "fail", "private_log_failed", "Could not create a private temporary helper log.")
		return r
	}
	defer os.Remove(output.Name())
	defer output.Close()
	ctx = naive.WithHelperOutput(ctx, output)
	log := logrus.New()
	log.SetOutput(io.Discard)
	h, err := naive.StartSingClient(ctx, &c, log)
	if err != nil {
		add("helper", "fail", "helper_start_failed", "The pinned helper failed local preflight or startup. Check its binary version and configuration with backpack check.")
		return r
	}
	defer h.Close()
	add("helper", "pass", "local_helper_ready", "Pinned helper preflight passed and its private loopback listener is ready; remote health is not yet proven.")
	proxy, err := url.Parse(h.ProxyURL())
	if err != nil {
		add("route", "fail", "invalid_local_proxy", "Could not read the private helper endpoint.")
		return r
	}
	routeCtx, stop := context.WithTimeout(ctx, 7*time.Second)
	raw, err := (&net.Dialer{}).DialContext(routeCtx, "tcp", proxy.Host)
	if err != nil {
		stop()
		add("route", "fail", "local_helper_unreachable", "The private helper listener could not be reached.")
		return r
	}
	defer raw.Close()
	closeOnCancel := context.AfterFunc(routeCtx, func() { raw.Close() })
	host, port, _ := net.SplitHostPort(c.RemoteAddr)
	number, _ := strconv.Atoi(port)
	err = socks.NegotiateWithin(raw, "", "", host, number, 7*time.Second)
	closeOnCancel()
	stop()
	if err != nil {
		code, detail := helperDiagnosticSignal(output)
		add("route", "fail", code, detail)
		add("authentication", "skipped", "not_confirmed", "No authenticated Reverse proof was received; a timeout cannot determine whether the credentials match.")
		return r
	}
	add("route", "pass", "socks_route_accepted", "The helper accepted a route to the configured private Reverse listener. Authentication is verified by the peer proof next.")
	proofCtx, stopProof := context.WithTimeout(ctx, 10*time.Second)
	defer stopProof()
	closeProof := context.AfterFunc(proofCtx, func() { raw.Close() })
	defer closeProof()
	if deadline, ok := proofCtx.Deadline(); ok {
		raw.SetDeadline(deadline)
	}
	conn := naive.WrapDuplex(raw)
	request, nonce, err := pathprobe.Request(c.Token, 16384)
	started := time.Now()
	if err == nil {
		err = utils.SendBinaryTransportString(conn, request, utils.SG_PathProbe)
	}
	var proof pathprobe.Result
	if err == nil {
		proof, err = pathprobe.Check(conn, c.Token, nonce, started)
	}
	if proof.Authenticated {
		add("authentication", "pass", "peer_authenticated", "The remote Reverse listener returned a valid token-bound proof through the actual carrier.")
	} else {
		code, detail := helperDiagnosticSignal(output)
		add("authentication", "fail", code, detail+" The Iran peer must also support diagnostic probes in this build.")
	}
	if err != nil {
		add("payload", "fail", "payload_not_verified", "The complete bounded upload and download could not be verified; the tunnel is not reported healthy.")
		return r
	}
	add("payload", "pass", "bidirectional_payload_verified", "Verified 16 KiB upload and 16 KiB download without claiming the control channel or opening a backend.")
	r.Verified = true
	if c.Sing.NativeUDP {
		udpCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		pc, err := naive.NewSOCKSPacketConn(udpCtx, proxy.Host, c.RemoteAddr)
		if err == nil {
			defer pc.Close()
			peer, _ := net.ResolveUDPAddr("udp", c.RemoteAddr)
			err = nativeudp.Probe(udpCtx, pc, peer, c.Token)
		}
		if err != nil {
			r.Verified = false
			add("native-udp", "fail", "native_datagrams_unverified", "The stream passed, but authenticated native datagrams did not verify. UDP forwarding is not healthy.")
		} else {
			add("native-udp", "pass", "native_datagrams_verified", "Authenticated native datagram probe succeeded through this carrier.")
		}
	} else {
		add("native-udp", "skipped", "native_udp_disabled", "Native reverse UDP is disabled in this configuration.")
	}
	return r
}
