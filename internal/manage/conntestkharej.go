package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/backpack/backpack/config"
)

// RunConnTestKharej is the kharej's side of a test: it builds its end of
// every tunnel in the link, checks in with the Iran side, waits for the
// verdict and prints it to out. live, when set, is handed the Iran side's rows
// every couple of seconds while the test runs. Everything it started is
// stopped before it returns.
func RunConnTestKharej(ctx context.Context, raw string, out io.Writer, live func([]ConnTestResult)) ([]ConnTestResult, ConnTestBest, error) {
	addr, err := parseConnTestLink(raw)
	if err != nil {
		return nil, ConnTestBest{}, err
	}
	if len(addr.Routes) > 0 {
		paths, err := runConnTestSuiteKharej(ctx, addr.Routes, out, nil)
		var rows []ConnTestResult
		for _, p := range paths {
			rows = append(rows, p.Rows...)
		}
		return rows, ConnTestSuiteBest(paths), err
	}
	fmt.Fprintf(out, "Fetching the test from %s...\n", addr.Host)
	fetchCtx, cancelFetch := context.WithTimeout(ctx, 3*time.Minute)
	link, err := fetchConnTestLink(fetchCtx, addr)
	cancelFetch()
	if err != nil {
		return nil, ConnTestBest{}, err
	}
	return runConnTestKharejLink(ctx, link, out, live)
}

func runConnTestKharejLink(ctx context.Context, link ConnTestLink, out io.Writer, live func([]ConnTestResult)) ([]ConnTestResult, ConnTestBest, error) {
	until := time.Unix(link.Until, 0)
	if time.Now().After(until) {
		return nil, ConnTestBest{}, fmt.Errorf("this test link has expired — start a new test on the Iran server")
	}
	ctx, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	root := os.Geteuid() == 0

	dir, err := os.MkdirTemp("", "backpack-conntest-")
	if err != nil {
		return nil, ConnTestBest{}, err
	}
	defer os.RemoveAll(dir)

	// The far ends of the tunnels: what the Iran side's echoes land on.
	echoes, err := ctStartEchoes(ctx, link)
	if err != nil {
		return nil, ConnTestBest{}, err
	}
	defer echoes.close()
	kernelPeer := &ctKernelPeer{}
	defer kernelPeer.close()
	if link.KernelTests {
		if err := kernelPeer.poll(ctx, &link, dir); err != nil {
			return nil, ConnTestBest{}, err
		}
	}

	var engines []*ctEngine
	defer func() { ctStopAll(engines) }()
	local := map[string]string{}  // kind/transport → why this side could not run it
	caseOf := map[string]string{} // engine name → kind/transport
	usedDirectPorts := map[int]bool{link.TCP: true, link.UDP: true, link.L3: true}
	for i, c := range link.Cases {
		name := c.Name
		key := ctLinkCaseKey(link, c)
		caseOf[name] = c.Kind + "/" + key
		var body string
		switch c.Kind {
		case "kernel":
			continue // created on demand by the serial phase
		case "awg":
			if !root {
				local[c.Kind+"/"+key] = "needs root and TUN"
				continue
			}
			peer, err := ctAWGKharej(ctx, link, c, name)
			if err != nil {
				local[c.Kind+"/"+key] = err.Error()
				continue
			}
			body, err = awgBody(peer)
			if err != nil {
				local[c.Kind+"/"+key] = err.Error()
				continue
			}
			echoes.l3(ctx, hostOf(peer.Address), link.L3)
		case "reverse":
			if connTestNeedsRoot(c.Tr) && !root {
				local[c.Kind+"/"+key] = "needs root"
				continue
			}
			var spec TunnelSpec
			var err error
			if managedTransport(c.Tr) {
				spec, err = ctManagedClient(ctx, link, c, dir)
			} else {
				spec, err = kharejFromLink(c, LinkApplyOptions{Host: link.Host})
				if c.Tr == "ssh" {
					spec.CarrierOptions.SSHPassword = ctCaseToken(link.Tok, "reverse", "ssh")
				}
			}
			if err != nil {
				local[c.Kind+"/"+key] = err.Error()
				if managedTransport(c.Tr) {
					_, _ = ctAskContext(ctx, link.Host, link.Coord, "skip "+link.Tok+" "+c.Tr)
				}
				continue
			}
			if managedTransport(c.Tr) {
				probe := ctProbeHTTPS(ctx, spec)
				// Advisory only: the real helper still starts and must carry all
				// echoes and speed traffic. Older coordinators ignore this command.
				_, _ = ctAskContext(ctx, link.Host, link.Coord, "https-probe "+link.Tok+" "+c.Tr+":"+probe)
			}
			spec.Name = name
			body = ctQuiet(spec.Render())
		case "direct":
			if !root {
				local[c.Kind+"/"+key] = "needs root"
				continue
			}
			form := MirrorForPeer(c)
			form.Name = name
			if link.DirectPorts && ctPortCarrier(key) {
				port, err := ctKharejDirectPort(ctx, link, key, usedDirectPorts)
				if err != nil {
					local[c.Kind+"/"+key] = err.Error()
					continue
				}
				form.TunnelPort = port
			}
			n := form.ToNewDirectTunnel()

			if config.SingDatagram(c.Tr) {
				native, err := ctSingListener(ctx, link, c.Tr, dir, name, usedDirectPorts)
				if err != nil {
					local[c.Kind+"/"+key] = err.Error()
					continue
				}
				n.Sing = native
			}
			if c.Tr == "masque" || c.Tr == "masque-h2" || c.Tr == "webtransport" || c.Tr == "connect-ip" {
				var err error
				n.Masque, err = ctMasqueListener(ctx, link, dir, name, usedDirectPorts, c.Tr)
				if err != nil {
					local[c.Kind+"/"+key] = err.Error()
					continue
				}
			}
			spec, err := n.spec()
			if err != nil {
				local[c.Kind+"/"+key] = err.Error()
				continue
			}
			spec.Name, spec.Iface = name, fmt.Sprintf("bpt%d", i)
			if ctFamily(link.Host) == "IPv6" {
				spec.Addr = net.JoinHostPort("::", addrPort(spec.Addr))
			}
			if _, body, err = directBodyFromSpec(spec); err != nil {
				local[c.Kind+"/"+key] = err.Error()
				continue
			}
			echoes.l3(ctx, strings.SplitN(spec.LocalIP, "/", 2)[0], link.L3)
		}
		e, err := startCTEngine(dir, name, body)
		if err != nil {
			local[c.Kind+"/"+key] = err.Error()
			if managedTransport(c.Tr) {
				_, _ = ctAskContext(ctx, link.Host, link.Coord, "skip "+link.Tok+" "+c.Tr)
			}
			continue
		}
		engines = append(engines, e)
	}
	fmt.Fprintf(out, "Started %d test tunnels to %s (preset %s). Checking in with the Iran server...\n",
		len(engines), link.Host, link.Preset)
	// Setup failures belong in skipped rows on both machines. Older Iran
	// versions understand only the managed-helper skip command.
	if link.DirectPorts {
		for key := range local {
			if ctSkippableCase(key) {
				_, _ = ctAskContext(ctx, link.Host, link.Coord, "skip "+link.Tok+" "+key)
			}
		}
	}

	// Checking in: every few seconds until the Iran side hears it.
	for {
		reply, err := ctAskContext(ctx, link.Host, link.Coord, "hello "+link.Tok)
		if err == nil && reply == "stopped" {
			return nil, ConnTestBest{}, errConnTestStopped
		}
		if err == nil && reply == "ok" {
			break
		}
		if ctx.Err() != nil {
			return nil, ConnTestBest{}, ctCoordinatorFailure(link.Host, link.Coord)
		}
		ctSleep(ctx, 3*time.Second)
	}
	if link.SpoofSrc != "" {
		if why := ctStrictRPFilter(); why != "" {
			local["spoof/"+ctSpoofToKharej] = why
			fmt.Fprintf(out, "  IP spoofing: %s\n", why)
		}
		if !root {
			local["spoof/"+ctSpoofToIran] = "sending forged probes needs root here"
		}
		ctKharejSpoof(ctx, link, root)
	}
	go ctKharejPMTU(ctx, link)
	if link.Mode == ConnTestQuick {
		fmt.Fprintln(out, "Quick test: 20s carrier traffic; 2s per serial kernel case, with verified download/upload for every healthy tunnel. Use Full for longer samples.")
	} else {
		fmt.Fprintln(out, "Full test: 60-second stability check, then download/upload for each healthy tunnel (about 8–12 seconds each, plus connection setup).")
	}

	// The verdict, and the rows as they fill until it comes.
	var lastResultPoll time.Time
	for {
		if link.KernelTests {
			if e := kernelPeer.poll(ctx, &link, dir); errors.Is(e, errConnTestStopped) {
				return nil, ConnTestBest{}, e
			}
		}
		activeKernel := link.KernelTests && kernelPeer.generation > 0
		delay := 500 * time.Millisecond
		if activeKernel {
			delay = 100 * time.Millisecond
		}
		ctSleep(ctx, delay)
		// Advance setup/teardown quickly without repeatedly compressing the
		// full result table or adding control work to the speed measurement.
		if activeKernel && time.Since(lastResultPoll) < 500*time.Millisecond {
			continue
		}
		lastResultPoll = time.Now()
		for _, e := range engines {
			// Kept for the verdict rather than printed: a line written here
			// would land in the middle of the live table.
			if gone, why := e.exited(); gone && local[caseOf[e.name]] == "" {
				local[caseOf[e.name]] = "the engine here stopped: " + why
			}
		}
		reply, err := ctAskContext(ctx, link.Host, link.Coord, "result "+link.Tok)
		if err == nil && reply == "stopped" {
			return nil, ConnTestBest{}, errConnTestStopped
		}
		if err == nil && strings.HasPrefix(reply, "live ") && live != nil {
			if raw, err := unGzipB64(strings.TrimPrefix(reply, "live ")); err == nil {
				var rows []ConnTestResult
				if json.Unmarshal(raw, &rows) == nil {
					live(rows)
				}
			}
			continue
		}
		if err == nil && strings.HasPrefix(reply, "done ") {
			raw, err := unGzipB64(strings.TrimPrefix(reply, "done "))
			if err != nil {
				return nil, ConnTestBest{}, fmt.Errorf("the verdict arrived damaged — read it on the Iran server")
			}
			var v ctVerdict
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, ConnTestBest{}, fmt.Errorf("the verdict arrived damaged — read it on the Iran server")
			}
			for i, r := range v.Results {
				if why := local[r.Kind+"/"+ctResultCaseKey(r)]; why != "" && r.Status != ctOK {
					v.Results[i].Detail = strings.TrimPrefix(v.Results[i].Detail+"; kharej: "+why, "; ")
				}
			}
			if link.AutoSetup {
				ctRememberSetupMaterial(link, dir)
			}
			return v.Results, v.Best, nil
		}
		if ctx.Err() != nil {
			return nil, ConnTestBest{}, fmt.Errorf("the verdict never arrived from the Iran server — read it there")
		}
	}
}

// ctEchoes are the kharej's echo servers.
type ctEchoes struct {
	mu      sync.Mutex
	closers []io.Closer
	closed  bool
}

func ctStartEchoes(ctx context.Context, link ConnTestLink) (*ctEchoes, error) {
	e := &ctEchoes{}
	tl, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(link.TCP)))
	if err != nil {
		return nil, fmt.Errorf("port %d is taken on this server — start a new test on the Iran server: %w", link.TCP, err)
	}
	e.add(tl)
	go ctServeTCPEcho(tl, e)
	ul, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(link.UDP)))
	if err != nil {
		e.close()
		return nil, fmt.Errorf("port %d is taken on this server — start a new test on the Iran server: %w", link.UDP, err)
	}
	e.add(ul)
	go ctServeUDPEcho(ul)
	return e, nil
}

// l3 starts an echo on a direct tunnel's own address, once its interface is
// up to bind it.
func (e *ctEchoes) l3(ctx context.Context, ip string, port int) {
	go func() {
		for ctx.Err() == nil {
			l, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
			if err == nil {
				e.add(l)
				ctServeTCPEcho(l, e)
				return
			}
			ctSleep(ctx, time.Second)
		}
	}()
}

func (e *ctEchoes) add(c io.Closer) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		c.Close()
		return
	}
	e.closers = append(e.closers, c)
}

func (e *ctEchoes) remove(c io.Closer) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, current := range e.closers {
		if current == c {
			e.closers = append(e.closers[:i], e.closers[i+1:]...)
			return
		}
	}
}

func (e *ctEchoes) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	for _, c := range e.closers {
		c.Close()
	}
	e.closers = nil
}

func ctServeTCPEcho(l net.Listener, owners ...*ctEchoes) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		for _, owner := range owners {
			owner.add(c)
		}
		go func() {
			defer c.Close()
			defer func() {
				for _, owner := range owners {
					owner.remove(c)
				}
			}()
			ctServeSpeedOrEcho(c)
		}()
	}
}

func ctServeUDPEcho(l net.PacketConn) {
	buf := make([]byte, 65535)
	for {
		n, from, err := l.ReadFrom(buf)
		if err != nil {
			return
		}
		_, _ = l.WriteTo(buf[:n], from)
	}
}

// ctIPv4 is host as an IPv4 address, resolving a name; empty when there is none.
func ctIPv4(host string) string {
	return ctIPv4Context(context.Background(), host)
}

func ctIPv4Context(ctx context.Context, host string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() != nil {
			return ip.String()
		}
		return ""
	}
	ips, _ := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	for _, ip := range ips {
		if ip.To4() != nil {
			return ip.String()
		}
	}
	return ""
}

// ctStrictRPFilter says why this server would drop the spoof carrier's
// packets on arrival, when a strict reverse-path filter would: a forged
// source fails the check and the kernel discards it before the carrier sees
// it. Empty when the filter is loose or off.
func ctStrictRPFilter() string {
	b, err := os.ReadFile("/proc/sys/net/ipv4/conf/all/rp_filter")
	if err != nil || strings.TrimSpace(string(b)) != "1" {
		return ""
	}
	return "rp_filter is strict (1) on this server, which drops forged sources on arrival — " +
		"run: sysctl -w net.ipv4.conf.all.rp_filter=2 (and on the receiving interface), then test again"
}

// ctShortErr is an error without the addresses a net error repeats.
func ctShortErr(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && strings.Contains(msg[:i], "->") {
		head, _, _ := strings.Cut(msg, ": ")
		return head + ": " + msg[i+2:]
	}
	return msg
}

// Check both protocols on the receiving machine. Iran's random port can be
// occupied by a completely unrelated service on Kharej, especially one with
// many UDP sockets. This check changes only the temporary test's endpoint.
func ctKharejDirectPort(ctx context.Context, link ConnTestLink, tr string, used map[int]bool) (string, error) {
	port := 0
	for attempt := 0; attempt < 10 && ctx.Err() == nil; attempt++ {
		if port == 0 {
			port = ctPickPort(used, true)
			if port == 0 {
				return "", errors.New("no free local listener port")
			}
		}
		line := fmt.Sprintf("port %s %s:%d", link.Tok, tr, port)
		reply, err := ctAskContext(ctx, link.Host, link.Coord, line)
		if err == nil && reply == "ok" {
			return strconv.Itoa(port), nil
		}
		if err == nil && reply == "busy" && tr == "dns" {
			port = 0 // try a different port; keep the rejected one reserved locally
			continue
		}
		if attempt >= 2 {
			break
		}
		ctSleep(ctx, time.Second)
	}
	return "", errors.New("could not announce the free Kharej listener port to Iran")
}
