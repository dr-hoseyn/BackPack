package manage

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/backpack/backpack/config"
)

// The coordinator: one question and one answer per connection, over TCP and
// over UDP, so that a path cutting flows after a few packets — or one that
// carries only one of the two — still lets the two sides talk.
//
//	config <token>  → config <c>  the test, for the kharej to build its end from
//	hello <token>   → ok          the kharej is in; its address is noted
//	result <token>  → wait | done <verdict>
type ctCoordinator struct {
	kernelWire                 string // cached for one immutable kernel generation
	kernelCommand              ctKernelCommand
	kernelAck                  ctKernelAck
	kernelCapable, kernelAcked bool
	tok                        string
	tcp                        net.Listener
	udp                        net.PacketConn
	mu                         sync.Mutex
	peer                       string
	verdict                    string
	additional                 string            // separately authenticated added direct carriers
	config                     string            // the answer to "config"; see ctConfig
	carriers                   map[string]string // public helper settings, fetched one small packet at a time
	httpsProbes                map[string]string // bounded preflight codes; frozen when Kharej joins
	skipped                    map[string]bool   // managed cases unavailable on kharej
	masqueSettings             string            // public certificate/target announced before the peer joins
	masqueH2Settings           string            // separate trust/target for the TCP fallback
	connectIPSettings          string
	webTransportSettings       string            // independent authenticated datagram trust
	directPorts                map[string]string // direct listener ports selected on Kharej
	setup                      *ConnTestSetup
	live                       func() []ConnTestResult
	joined                     chan struct{}
	fetched                    chan struct{}
	joinOne                    sync.Once
	fetchOn                    sync.Once
	sockets                    ctEchoes
	slots                      chan struct{}
	// spoofArrived is the kharej's count of the Iran server's forged probes.
	spoofArrived chan int
	spoofOnce    sync.Once
	// pmtu is the path MTU the kharej measured.
	pmtu     chan int
	pmtuOnce sync.Once

	// Installed before the suite link is exposed; immutable thereafter.
	routeUnavailable func(int) bool
	stopped          bool
}

func startCTCoordinator(port int, tok string) (*ctCoordinator, error) {
	c := &ctCoordinator{tok: tok, joined: make(chan struct{}), fetched: make(chan struct{}), slots: make(chan struct{}, 128),
		spoofArrived: make(chan int, 1), pmtu: make(chan int, 1)}
	var err error
	if c.tcp, err = net.Listen("tcp", fmt.Sprintf(":%d", port)); err != nil {
		return nil, err
	}
	if c.udp, err = net.ListenPacket("udp", fmt.Sprintf(":%d", port)); err != nil {
		c.tcp.Close()
		return nil, err
	}
	go c.serveTCP()
	go c.serveUDP()
	return c, nil
}

func (c *ctCoordinator) serveTCP() {
	for {
		conn, err := c.tcp.Accept()
		if err != nil {
			return
		}
		select {
		case c.slots <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		c.sockets.add(conn)
		go func() {
			defer func() { <-c.slots; c.sockets.remove(conn) }()
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
			packet, err := ctReadPacketLimit(conn, 8192)
			if err != nil {
				return
			}
			plain, nonce, err := ctOpen(c.tok, "request", nil, packet)
			if err != nil {
				return
			}
			line := string(plain)
			host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
			if reply := c.answer(line, host); reply != "" {
				wire, _, err := ctSeal(c.tok, "reply", nonce, []byte(reply))
				if err != nil {
					return
				}
				err = ctWritePacket(conn, wire)
				if err == nil && (strings.HasPrefix(reply, "done ") || reply == "stopped" && ctStopAcknowledged(line)) {
					// The peer reads to EOF. Publish Fetched only after its
					// full verdict has left this socket; a suite immediately
					// tears down this path before starting the next one.
					conn.Close()
					c.fetchOn.Do(func() { close(c.fetched) })
				}
			}
		}()
	}
}

func (c *ctCoordinator) serveUDP() {
	buf := make([]byte, 8192) // bounded setup requests and path-MTU probes
	for {
		n, from, err := c.udp.ReadFrom(buf)
		if err != nil {
			return
		}
		host, _, _ := net.SplitHostPort(from.String())
		plain, nonce, err := ctOpen(c.tok, "request", nil, buf[:n])
		if err != nil {
			continue
		}
		line := string(plain)
		if reply := c.answer(line, host); reply != "" {
			wire, _, err := ctSeal(c.tok, "reply", nonce, []byte(reply))
			if err != nil {
				continue
			}
			n, err := c.udp.WriteTo(wire, from)
			if err == nil && n == len(wire) && (strings.HasPrefix(reply, "done ") || reply == "stopped" && ctStopAcknowledged(line)) {
				c.fetchOn.Do(func() { close(c.fetched) })
			}
		}
	}
}

// Only the worker's lifecycle requests observe cancellation and return from
// the run. A background PMTU probe ignores other replies; counting it as a
// shutdown acknowledgement would close the coordinator before the worker hears.
func ctStopAcknowledged(line string) bool {
	f := strings.Fields(line)
	return len(f) >= 2 && (f[0] == "config" || f[0] == "additional" || f[0] == "hello" || f[0] == "result" || f[0] == "kernel-next")
}

// answer is the coordinator's whole protocol. An empty answer is silence: a
// request without the token gets nothing, not even a refusal.
//
//	spoof <token> <n>  → ok      the kharej counted n forged probes
//	pmtu <token> <n> <pad> → pm <n>  a path-MTU probe of n bytes
//	pmtu <token> <n>   → ok      the path MTU the kharej measured
func (c *ctCoordinator) answer(line, from string) string {
	f := strings.Fields(line)
	if len(f) < 2 || subtle.ConstantTimeCompare([]byte(f[1]), []byte(c.tok)) != 1 {
		return ""
	}
	c.mu.Lock()
	stopped := c.stopped
	c.mu.Unlock()
	if stopped {
		return "stopped"
	}
	// A path-MTU probe: its padding is the point, and the answer is small.
	if f[0] == "kernel-next" || f[0] == "kernel-ack" {
		return c.kernelAnswer(f)
	}
	if f[0] == "pmtu" && len(f) == 4 {
		return "pm " + f[2]
	}
	if len(f) > 3 {
		return ""
	}
	if strings.HasPrefix(f[0], "setup-") {
		if (f[0] == "setup-poll" && len(f) != 2) ||
			(f[0] != "setup-poll" && (len(f) != 3 || (f[0] != "setup-start" && f[0] != "setup-ack"))) {
			return ""
		}
		c.mu.Lock()
		setup := c.setup
		c.mu.Unlock()
		if setup == nil {
			if f[0] != "setup-poll" {
				return "error automatic setup is not ready yet"
			}
			return "setup " + ctSetupEncode(ctSetupWire{View: ConnTestSetupView{State: "starting"}})
		}
		payload := ""
		if len(f) == 3 {
			payload = f[2]
		}
		return setup.answer(f[0], payload)
	}
	if f[0] == "sing-cert" && len(f) == 3 {
		return ctAcceptSingIdentity(c, f[2])
	}
	if (f[0] == "masque-cert" || f[0] == "masque-h2-cert" || f[0] == "webtransport-cert" || f[0] == "connect-ip-cert") && len(f) == 3 {
		if _, err := ctDecodeMasqueSettings(f[2]); err != nil {
			return ""
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		settings := &c.masqueSettings
		if f[0] == "masque-h2-cert" {
			settings = &c.masqueH2Settings
		}
		if f[0] == "webtransport-cert" {
			settings = &c.webTransportSettings
		}
		if f[0] == "connect-ip-cert" {
			settings = &c.connectIPSettings
		}
		if c.peer != "" || *settings != "" && *settings != f[2] {
			return ""
		}
		*settings = f[2]
		return "ok"
	}
	if f[0] == "https-probe" && len(f) == 3 {
		tr, probe, ok := strings.Cut(f[2], ":")
		if !ok || !ctValidHTTPSProbe(tr, probe) {
			return ""
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.peer != "" {
			return "" // late/replayed reports must not alter a running measurement
		}
		if c.httpsProbes == nil {
			c.httpsProbes = make(map[string]string)
		}
		c.httpsProbes[tr] = probe
		return "ok"
	}
	if f[0] == "unavailable" && len(f) == 3 {
		c.mu.Lock()
		unavailable := c.routeUnavailable
		c.mu.Unlock()
		i, err := strconv.Atoi(f[2])
		if err == nil && unavailable != nil && unavailable(i) {
			return "ok"
		}
		return ""
	}
	if f[0] == "port" && len(f) == 3 {
		tr, port, ok := strings.Cut(f[2], ":")
		n, err := strconv.Atoi(port)
		if !ok || err != nil || n < 1 || n > 65535 || !ctPortCarrier(tr) {
			return ""
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		// Once the peer has checked in, an old/replayed request must not move
		// the endpoint underneath the running measurement.
		if c.peer != "" && c.directPorts[tr] != port {
			return ""
		}
		// DNS receives on the shared fixed port at BOTH endpoints. A port
		// free on Kharej can still belong to Iran's resolver or another service.
		if tr == "dns" && c.peer == "" && c.directPorts[tr] != port {
			socket, bindErr := net.ListenPacket("udp", net.JoinHostPort("", port))
			if bindErr != nil {
				return "busy"
			}
			_ = socket.Close()
		}
		if c.directPorts == nil {
			c.directPorts = make(map[string]string)
		}
		c.directPorts[tr] = port
		return "ok"
	}
	if f[0] == "skip" && len(f) == 3 && ctSkippableCase(f[2]) {
		c.mu.Lock()
		if c.skipped == nil {
			c.skipped = make(map[string]bool)
		}
		c.skipped[f[2]] = true
		c.mu.Unlock()
		return "ok"
	}
	if f[0] == "carrier" && len(f) == 3 {
		c.mu.Lock()
		body := c.carriers[f[2]]
		c.mu.Unlock()
		if body != "" {
			return "carrier " + body
		}
		return ""
	}
	if f[0] == "pmtu" {
		n, err := strconv.Atoi(f[len(f)-1])
		if len(f) != 3 || err != nil || n < 0 {
			return ""
		}
		c.pmtuOnce.Do(func() { c.pmtu <- n })
		return "ok"
	}
	if f[0] == "spoof" {
		n, err := strconv.Atoi(f[len(f)-1])
		if len(f) != 3 || err != nil || n < 0 {
			return ""
		}
		// Asked again when an answer was lost; the first count is the count.
		c.spoofOnce.Do(func() { c.spoofArrived <- n })
		return "ok"
	}
	if len(f) != 2 {
		return ""
	}
	switch f[0] {
	case "additional":
		c.mu.Lock()
		extra := c.additional
		c.mu.Unlock()
		if extra == "" {
			return ""
		}
		return "additional " + extra
	case "config":
		c.mu.Lock()
		cfg := c.config
		c.mu.Unlock()
		if cfg == "" {
			return ""
		}
		return "config " + cfg
	case "hello":
		c.mu.Lock()
		if c.peer == "" {
			c.peer = strings.TrimPrefix(from, "::ffff:")
		}
		c.mu.Unlock()
		c.joinOne.Do(func() { close(c.joined) })
		return "ok"
	case "result":
		c.mu.Lock()
		v, live := c.verdict, c.live
		c.mu.Unlock()
		if v == "" {
			if live == nil {
				return "wait"
			}
			raw, _ := json.Marshal(live())
			return "live " + gzipB64(raw)
		}
		return "done " + v
	}
	return ""
}

func (c *ctCoordinator) peerAddr() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peer
}

func (c *ctCoordinator) directPort(tr, fallback string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if port := c.directPorts[tr]; port != "" {
		return port
	}
	return fallback
}

func ctPortCarrier(tr string) bool {
	return tr == "dns" || tr == "udp" || tr == "quic" || tr == "pck" || tr == "sni" || tr == "masque" || tr == "masque-h2" || tr == "webtransport" || tr == "connect-ip" || config.SingDatagram(tr) || ctSNIVariantKey(tr)
}

func ctSkippableCase(value string) bool {
	if value == "awg/awg" {
		return true
	}
	if managedTransport(value) {
		return true
	} // old managed-helper command
	kind, tr, ok := strings.Cut(value, "/")
	if !ok {
		return false
	}
	var cases []string
	switch kind {
	case "direct":
		if ctSNIVariantKey(tr) {
			return true
		}
		cases = connTestDirect
	case "reverse":
		cases = connTestReverse
	default:
		return false
	}
	for _, candidate := range cases {
		if tr == candidate {
			return true
		}
	}
	return false
}

func (c *ctCoordinator) setTestConfig(l ConnTestLink) {
	main, extra := ctConfigParts(l)
	c.mu.Lock()
	c.config, c.additional = main, extra
	c.mu.Unlock()
}

// setConfig sets what the kharej is told the test is.
func (c *ctCoordinator) setConfig(cfg string) {
	c.mu.Lock()
	c.config = cfg
	c.mu.Unlock()
}

// setLive hands the coordinator the running test's rows, for the kharej's
// screen.
func (c *ctCoordinator) setLive(rows func() []ConnTestResult) {
	c.mu.Lock()
	c.live = rows
	c.mu.Unlock()
}

// ctVerdict is what the kharej fetches when the test is over.
type ctVerdict struct {
	Results []ConnTestResult `json:"r"`
	Best    ConnTestBest     `json:"b"`
}

func (c *ctCoordinator) publish(results []ConnTestResult, best ConnTestBest) {
	raw, _ := json.Marshal(ctVerdict{Results: results, Best: best})
	c.mu.Lock()
	c.verdict = gzipB64(raw)
	c.mu.Unlock()
}

func (c *ctCoordinator) close() {
	c.tcp.Close()
	c.udp.Close()
	c.sockets.close()
}
