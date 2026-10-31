package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tunnel/kernel"
)

const ctKernelQuickSamples = 4

func ctKernelMethod(tr string) (kernel.Method, bool, bool) {
	reverse := strings.HasSuffix(tr, "-reverse")
	name := strings.TrimSuffix(tr, "-reverse")
	m, ok := kernel.Lookup(name)
	return m, reverse, ok
}

// Kernel cases run serially. Unkeyed tunnels and IPsec selectors can otherwise
// intercept another case's packets and turn a successful echo into false proof.
func (s *ConnTestIran) addKernelCases(o ConnTestOptions) {
	if !o.Direct || o.skipKernel {
		return
	}
	s.link.KernelTests = true
	for _, reverse := range []bool{false, true} {
		for _, method := range kernel.Methods() {
			tr := method.Name
			if reverse {
				tr += "-reverse"
			}
			c := &connTestCase{kind: "kernel", tr: tr, name: "ct-" + s.link.ID + "-k-" + tr}
			c.link = ShareLink{Kind: c.kind, Tr: tr, From: "iran", Name: c.name, Tok: ctCaseToken(s.link.Tok, "kernel", tr)}
			if !s.root {
				c.skip = "kernel tests require root on both servers"
			}
			if !reverse && method.Name != "wireguard" && method.IPv6 != (ctFamily(s.link.Host) == "IPv6") {
				c.skip = "native method needs a different underlay IP family; use the matching test path"
			}
			s.cases = append(s.cases, c)
		}
	}
}

type ctKernelCommand struct {
	Generation int                  `json:"g"`
	Transport  string               `json:"t,omitempty"`
	Name       string               `json:"n,omitempty"`
	Config     *config.KernelConfig `json:"c,omitempty"`
}
type ctKernelAck struct {
	Generation int    `json:"g"`
	Error      string `json:"e,omitempty"`
}

// Called only after the coordinator has authenticated the run token.
func (c *ctCoordinator) kernelAnswer(fields []string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch fields[0] {
	case "kernel-next":
		if len(fields) != 2 {
			return ""
		}
		c.kernelCapable = true
		raw, _ := json.Marshal(c.kernelCommand)
		return "kernel " + gzipB64(raw)
	case "kernel-ack":
		if len(fields) != 3 {
			return ""
		}
		var ack ctKernelAck
		if ctSetupDecode(fields[2], &ack) != nil || ack.Generation != c.kernelCommand.Generation || len(ack.Error) > 512 {
			return ""
		}
		if !c.kernelAcked {
			c.kernelAck, c.kernelAcked = ack, true
		}
		return "ok"
	}
	return ""
}
func (c *ctCoordinator) kernelSet(tr, name string, cfg *config.KernelConfig) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kernelCommand.Generation++
	c.kernelCommand.Transport, c.kernelCommand.Name, c.kernelCommand.Config = tr, name, cfg
	c.kernelAcked = false
	return c.kernelCommand.Generation
}
func (c *ctCoordinator) kernelWait(ctx context.Context, generation int) error {
	for ctx.Err() == nil {
		c.mu.Lock()
		ack, done := c.kernelAck, c.kernelAcked
		c.mu.Unlock()
		if done && ack.Generation == generation {
			if ack.Error != "" {
				return errors.New(ack.Error)
			}
			return nil
		}
		ctSleep(ctx, 100*time.Millisecond)
	}
	return fmt.Errorf("kernel peer did not acknowledge generation %d: %w", generation, ctx.Err())
}

// Resolve the route's local source, rather than binding a public/NAT address
// that might not be assigned to this machine. No packets are sent by UDP dial.
func ctKernelLocal(ctx context.Context, peer string) (string, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, "udp", net.JoinHostPort(peer, "9"))
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String(), nil
}
func ctKernelPrerequisite(c config.KernelConfig) error {
	if os.Geteuid() != 0 {
		return errors.New("kernel tests require root")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		return errors.New("iproute2 is not installed")
	}
	if c.Method == "wireguard" {
		if _, err := exec.LookPath("wg"); err != nil {
			return errors.New("wireguard-tools is not installed")
		}
	}
	if c.Method == "xfrm-ipsec" {
		conn, err := net.DialTimeout("unix", "/run/charon.vici", time.Second)
		if err != nil {
			return errors.New("strongSwan/charon VICI is not available")
		}
		_ = conn.Close()
	}
	// Include private reverse/SIT underlay addresses, not only the user-facing
	// tunnel subnet: a helper route may otherwise overlap an existing network.
	plan, err := kernel.Build(c)
	if err != nil {
		return err
	}
	used := kernelUsedPrefixes()
	for _, step := range plan.Steps {
		if len(step.Args) < 3 || step.Args[0] != "address" || step.Args[1] != "add" {
			continue
		}
		for _, prefix := range used {
			if kernelAddressConflict(config.KernelConfig{LocalIP: step.Args[2]}, config.KernelConfig{LocalIP: prefix.String()}) {
				return errors.New("test subnet or helper route overlaps an existing tunnel or interface")
			}
		}
	}
	if c.Method == "xfrm-ipsec" && !c.Reverse {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		raw, err := exec.CommandContext(ctx, "ip", "-j", "-d", "link", "show").Output()
		if err != nil {
			return errors.New("cannot check ownership of native GRE/IPsec selectors")
		}
		var links []struct {
			LinkInfo struct {
				Data struct {
					Local  string `json:"local"`
					Remote string `json:"remote"`
				} `json:"info_data"`
			} `json:"linkinfo"`
		}
		if json.Unmarshal(raw, &links) != nil {
			return errors.New("cannot inspect native IPsec selectors")
		}
		for _, l := range links {
			if l.LinkInfo.Data.Local == c.LocalAddr && l.LinkInfo.Data.Remote == c.RemoteAddr {
				return errors.New("native IPsec underlay pair already has a tunnel; selectors must be exclusive")
			}
		}
	}
	return nil
}

// Kernel settings/acks fit a datagram. Prefer UDP so a blocked coordinator TCP
// port does not spend its dial timeout on every serial phase transition. Retain
// the standard TCP/UDP fallback for routes that block the datagram exchange.
func ctKernelAsk(ctx context.Context, link ConnTestLink, line string) (string, error) {
	probe, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(probe, "udp", net.JoinHostPort(link.Host, strconv.Itoa(link.Coord)))
	if err == nil {
		stop := context.AfterFunc(probe, func() { _ = conn.Close() })
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		_, err = conn.Write([]byte(line + "\n"))
		if err == nil {
			var packet [1201]byte
			n, readErr := conn.Read(packet[:])
			if readErr == nil && n > 0 && n < len(packet) {
				stop()
				_ = conn.Close()
				return strings.TrimSpace(string(packet[:n])), nil
			}
		}
		stop()
		_ = conn.Close()
	}
	return ctAskContext(ctx, link.Host, link.Coord, line)
}

type ctKernelRuntime struct {
	cancel context.CancelFunc
	done   chan error
	echoes *ctEchoes
}

func ctStartKernel(ctx context.Context, c config.KernelConfig, echoPort int) (*ctKernelRuntime, error) {
	if err := ctKernelPrerequisite(c); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	r := &ctKernelRuntime{cancel: cancel, done: make(chan error, 1), echoes: &ctEchoes{}}
	ready := make(chan struct{}, 1)
	go func() {
		r.done <- kernel.RunManaged(runCtx, c, func(up bool) {
			if up {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
		})
	}()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-ready:
		if echoPort > 0 {
			r.echoes.l3(runCtx, hostOf(c.LocalIP), echoPort)
		}
		return r, nil
	case err := <-r.done:
		cancel()
		if err == nil {
			err = errors.New("kernel engine stopped before readiness")
		}
		return nil, err
	case <-timer.C:
		cancel()
		err := <-r.done
		return nil, errors.Join(errors.New("kernel local startup timed out"), err)
	case <-ctx.Done():
		cancel()
		err := <-r.done
		return nil, errors.Join(ctx.Err(), err)
	}
}
func (r *ctKernelRuntime) stop() error {
	if r == nil {
		return nil
	}
	r.cancel()
	r.echoes.close()
	return <-r.done
}
func ctKernelConfigFile(dir, name string, c config.KernelConfig) error {
	body, err := kernelBody(c)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".toml"), []byte(body), 0600)
}

// A peer owns at most one kernel generation. Replays return the original ack;
// advancing a generation joins cleanup before any new network state is created.
type ctKernelPeer struct {
	lastContact time.Time

	generation int
	ack        ctKernelAck
	runtime    *ctKernelRuntime
}

func (p *ctKernelPeer) close() error { err := p.runtime.stop(); p.runtime = nil; return err }
func (p *ctKernelPeer) poll(ctx context.Context, link *ConnTestLink, dir string) error {
	pollCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	reply, err := ctKernelAsk(pollCtx, *link, "kernel-next "+link.Tok)
	cancel()
	if err != nil {
		if p.runtime != nil && !p.lastContact.IsZero() && time.Since(p.lastContact) > 25*time.Second {
			cleanupErr := p.close()
			p.ack.Error = "kernel control lease expired"
			return errors.Join(err, cleanupErr)
		}
		return err
	}
	p.lastContact = time.Now()
	if reply == "stopped" {
		return errConnTestStopped
	}
	if !strings.HasPrefix(reply, "kernel ") {
		return errors.New("kernel test coordinator is unavailable")
	}
	raw, err := unGzipB64(strings.TrimPrefix(reply, "kernel "))
	if err != nil {
		return err
	}
	var command ctKernelCommand
	if json.Unmarshal(raw, &command) != nil || command.Generation < 0 {
		return errors.New("invalid kernel generation")
	}
	if command.Generation == 0 {
		return nil
	}
	if command.Generation < p.generation {
		return errors.New("stale kernel generation")
	}
	if command.Generation > p.generation {
		p.generation = command.Generation
		p.ack = ctKernelAck{Generation: p.generation}
		err = p.close()
		if err == nil && command.Config != nil {
			c := *command.Config
			method, rev, ok := ctKernelMethod(command.Transport)
			if !ok || c.Method != method.Name || c.Reverse != rev || c.Role != "kharej" || !validName(command.Name) || !strings.HasPrefix(command.Name, "ct-") || len(c.Ports) > 0 || c.Token != "" {
				err = errors.New("invalid kernel test configuration")
			} else {
				c.Token = ctCaseToken(link.Tok, "kernel", command.Transport)
				c.LocalAddr, err = ctKernelLocal(ctx, link.Host)
				if err == nil {
					c, err = kernel.Resolve(c)
				}
				if err == nil {
					p.runtime, err = ctStartKernel(ctx, c, link.L3)
				}
				if err == nil {
					err = ctKernelConfigFile(dir, command.Name, c)
				}
				if err == nil {
					found := false
					for _, old := range link.Cases {
						if old.Kind == "kernel" && old.Tr == command.Transport {
							found = true
						}
					}
					if !found {
						link.Cases = append(link.Cases, ShareLink{Kind: "kernel", From: "iran", Tr: command.Transport, Name: command.Name})
					}
				}
			}
		}
		if err != nil {
			p.ack.Error = ctShortErr(err)
			if len(p.ack.Error) > 512 {
				p.ack.Error = p.ack.Error[:512]
			}
		}
	}
	ack, err := ctKernelAsk(ctx, *link, "kernel-ack "+link.Tok+" "+ctSetupEncode(p.ack))
	if err == nil && ack != "ok" {
		return errors.New("kernel acknowledgement was refused")
	}
	return err
}

func (s *ConnTestIran) kernelConfig(ctx context.Context, c *connTestCase, peer string) (config.KernelConfig, error) {
	method, rev, ok := ctKernelMethod(c.tr)
	if !ok {
		return config.KernelConfig{}, errors.New("unknown kernel method")
	}
	local, err := ctKernelLocal(ctx, peer)
	if err != nil {
		return config.KernelConfig{}, err
	}
	remote, err := ctResolveHost(ctx, peer, "")
	if err != nil {
		return config.KernelConfig{}, err
	}
	// Subnets are temporary and allocated against all configured/live interfaces.
	inner, other := suggestKernelPair(method.IPv6Only)
	cfg := config.KernelConfig{Method: method.Name, Reverse: rev, Role: "iran", LocalAddr: local, RemoteAddr: remote, LocalIP: inner, PeerIP: other, MTU: 1280, Token: ctCaseToken(s.link.Tok, "kernel", c.tr)}
	cfg, err = kernel.Resolve(cfg)
	if err != nil {
		return cfg, err
	}
	if cfg.Port > 0 {
		// IKE ports remain 500/4500; all configurable carriers use a fresh port.
		cfg.Port = ctPickPort(map[int]bool{s.link.Coord: true}, true)
		if cfg.Port == 0 {
			return cfg, errors.New("no free kernel test port")
		}
	}
	if err = ctKernelPrerequisite(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (s *ConnTestIran) runKernelCase(ctx context.Context, c *connTestCase, report func(ConnTestResult)) (r ConnTestResult) {
	r = c.initialResult()
	fail := func(err error) ConnTestResult {
		r.Status, r.Detail, r.Phase = ctSkipped, ctShortErr(err), ""
		report(r)
		return r
	}
	if s.kernelFailure != "" {
		return fail(errors.New("previous kernel cleanup failed: " + s.kernelFailure))
	}
	s.coord.mu.Lock()
	capable := s.coord.kernelCapable
	s.coord.mu.Unlock()
	if !capable {
		return fail(errors.New("update Kharej: this worker does not support kernel Connection Tests"))
	}
	if ctx.Err() != nil {
		return fail(ctx.Err())
	}
	cfg, err := s.kernelConfig(ctx, c, s.Kharej())
	if err != nil {
		return fail(err)
	}
	peer := kernel.Peer(cfg)
	peer.RemoteAddr, err = ctResolveHost(ctx, s.link.Host, "")
	if err != nil {
		return fail(err)
	}
	peer.Token = ""
	generation := s.coord.kernelSet(c.tr, c.name, &peer)
	var runtime *ctKernelRuntime
	defer func() {
		localErr := runtime.stop()
		generation = s.coord.kernelSet("", "", nil)
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		peerErr := s.coord.kernelWait(cleanup, generation)
		cancel()
		if e := errors.Join(localErr, peerErr); e != nil {
			s.kernelFailure = ctShortErr(e)
			r.Status, r.Detail, r.Phase = ctUnstable, "kernel cleanup failed: "+s.kernelFailure, ""
		}
		report(r)
	}()
	wait, cancel := context.WithTimeout(ctx, 20*time.Second)
	err = s.coord.kernelWait(wait, generation)
	cancel()
	if err != nil {
		return fail(fmt.Errorf("Kharej local setup: %w", err))
	}
	runtime, err = ctStartKernel(ctx, cfg, 0)
	if err != nil {
		return fail(err)
	}
	c.peerIP = cfg.PeerIP
	c.kernel = cfg
	c.link.Port = strconv.Itoa(cfg.Port)
	if err = ctKernelConfigFile(s.dir, c.name, cfg); err != nil {
		return fail(err)
	}
	r = ctProbe(ctx, c, s.link.L3, report)
	if r.Phase == "speed-queued" {
		r.Phase, r.Detail = "speed", "measuring kernel download and upload"
		report(r)
		speed, e := ctSpeedForMode(ctx, func(dialCtx context.Context) (net.Conn, error) { return ctDial(dialCtx, c, s.link.L3) }, s.link.Mode)
		r.Phase, r.Detail = "", ""
		if e != nil {
			r.Status, r.Detail = ctUnstable, "speed test failed ("+ctShortErr(e)+")"
		} else {
			r.Status, r.Mbps, r.UploadMbps, r.SpeedStable = ctOK, speed.download, speed.upload, speed.stable
		}
	}
	return r
}

// ICMP validates remote reachability for promoted native interfaces, whose local
// readiness must never be mistaken for an authenticated reverse control peer.
func ctVerifyKernel(ctx context.Context, c config.KernelConfig) error {
	for ctx.Err() == nil {
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := exec.CommandContext(probe, "ping", "-n", "-c", "1", "-W", "1", "-I", c.Interface, c.PeerIP).Run()
		cancel()
		if err == nil {
			return nil
		}
		ctSleep(ctx, time.Second)
	}
	return fmt.Errorf("kernel peer did not carry a ping: %w", ctx.Err())
}
