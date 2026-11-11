package manage

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/carrier"
	"github.com/backpack/backpack/internal/spooftest"
	"github.com/backpack/backpack/internal/tunnel/naive"
	"github.com/klauspost/compress/gzip"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

// Connection Test: which transports survive the path between two servers.
//
// Reported from the field: a reverse tunnel between one Iran server and one
// kharej came up and fell over every seventeen seconds. Nothing in either
// configuration was wrong. The path let every new flow carry three to five
// packets and then dropped it — TCP, UDP, TLS and plain HTTP alike — and the
// only way to find that out was a round of hand-written Python on both
// machines. Which transport, if any, survives a given pair of servers is a
// property of the route, not of the program, and the only honest way to know
// is to run each one across it.
//
// So this runs them. The Iran side starts a real engine for every transport,
// on free ports, with throwaway configs, and prints a link. The kharej, given
// the link, builds its own end of each one the way `link apply` builds a real
// tunnel and starts them too. Then the Iran side pushes traffic through every
// tunnel — an echo a second for a minute, and a bulk transfer at the end — and
// reports which carried it all, which dropped, and which never came up. Both
// sides print the table, and nothing is left behind: no unit, no config under
// /etc/backpack, no rule once the engines have stopped.
//
// The Iran side is the judge because it is where users arrive: every tunnel,
// reverse or direct, is used from there. The kharej hears the verdict through
// a small coordinator on the Iran server — one short exchange per question, so
// a path that cuts flows after a few packets still carries it — which is also
// how the Iran side learns the kharej's address for the direct tunnels, whose
// Iran end dials out.

// connTestKind marks a test link. A setup link is "reverse" or "direct".
const connTestKind = "test"

// Which transports a test covers: every one the wizards offer. Spoof joins
// the direct ones when the operator names a source to forge (see
// ConnTestOptions.SpoofSrc); pck, on either side, needs root on both servers.
var (
	connTestReverse = []string{"tls", "grpc", "dns", "ssh", "tcp", "tcpmux", "stealth", "pck", "ws", "wss", "wsmux", "wssmux", "kcp", "xdi", "quic", "udp", "naive", "short-https", "xhttp", "reality", "hysteria2", "anytls", "shadowtls", "tuic", "xhttp-reality", "trusttunnel", "webtransport", "tailcat"}
	connTestDirect  = []string{"dns", "icmp-request", "icmp-reply", "udp", "quic", "pck", "xdi", "sni", "masque", "masque-h2", "hysteria2", "tuic", "webtransport", "connect-ip", "spoof"}
)

// connTestNeedsRoot is a reverse transport that builds its own packets.
func connTestNeedsRoot(tr string) bool { return tr == "pck" || tr == "xdi" }

// Variables rather than constants so a test can run the whole thing in
// seconds.
var (
	// connTestJoinWait is how long the Iran side waits for the kharej.
	connTestJoinWait = 15 * time.Minute
	// connTestConnectWait is how long a tunnel has to carry its first echo.
	connTestConnectWait = 60 * time.Second
	// connTestSoak is how many one-second echoes a tunnel must carry. Long
	// enough for a path that cuts flows after a few seconds to show it.
	connTestSoak = 60
	// connTestSlack is how long past its schedule the kharej keeps waiting
	// for the verdict before it stops on its own.
	connTestSlack    = 5 * time.Minute
	connTestStopWait = 8 * time.Second
)

// connTestBulk is the legacy echo payload, retained for compatibility checks.
// Run measures sustained directional goodput with ctSpeed instead.
const connTestBulk = 1 << 20

// connTestBinary is the engine to run; a variable so a test can point it at a
// built binary rather than at the test itself.
var connTestBinary = os.Executable

var errConnTestStopped = errors.New("the Iran server stopped this connection test")

// Use the same pinned helper binaries as the setup wizard. Absence is a
// skipped test, rather than evidence that the network blocked a protocol.
var connTestHelperBinary = func(tool string) (string, error) {
	path := managedHelperPath(tool)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("%s helper is not installed at %s; reinstall with BP_HELPERS=naive,xray", tool, path)
	}
	return path, nil
}

// ConnTestLink is everything the kharej needs to build its end of every
// tunnel under test, and to find the Iran side's coordinator.
type ConnTestLink struct {
	KernelTests      bool   `json:"kt,omitempty"` // serial kernel phase, negotiated with updated workers
	AdditionalDirect int    `json:"ad,omitempty"` // new carriers fetched in a second bounded answer
	Mode             string `json:"mode,omitempty"`
	AutoSetup        bool   `json:"as,omitempty"`
	V                int    `json:"v"`
	Kind             string `json:"k"` // always connTestKind
	ID               string `json:"id"`
	Host             string `json:"h"`  // the Iran server's address
	Coord            int    `json:"c"`  // the coordinator's port, TCP and UDP
	Tok              string `json:"t"`  // the coordinator's secret
	TCP              int    `json:"te"` // kharej: the TCP echo reverse tunnels forward to
	UDP              int    `json:"ue"` // kharej: the UDP echo the udp transport forwards to
	L3               int    `json:"le"` // kharej: the TCP echo on each direct tunnel's address
	Until            int64  `json:"u"`  // unix seconds: the kharej stops by then whatever happens
	// Preset is the performance preset every tunnel under test was built
	// with, for the kharej's report; each case carries it too.
	Preset string `json:"pr,omitempty"`
	// New peers may choose a direct listener port on the machine that binds it.
	// An older peer keeps the port picked on Iran, as before.
	DirectPorts bool `json:"dp,omitempty"`
	// SNI variants use domain-qualified identities for independent ports/tokens.
	SNIVariants bool `json:"sv,omitempty"`
	// The spoofing check: the forged source, and the UDP port each side's
	// receiver counts probes on. Empty SpoofSrc is no check.
	SpoofSrc string `json:"ss,omitempty"`
	SpoofK   int    `json:"sk,omitempty"` // the kharej's receiver
	SpoofI   int    `json:"si,omitempty"` // the Iran server's receiver
	SpoofN   int    `json:"sn,omitempty"` // probes each way
	// Cases are the setup links of the tunnels under test, exactly as the
	// wizard would print them, so the kharej builds each one by the path a
	// real tunnel takes.
	Cases []ShareLink `json:"x"`
}

// The link the operator copies is only where the Iran side's coordinator is
// and the test's secret: backpack://t. and some twenty-seven characters. The
// rest — every tunnel's settings — the kharej asks the coordinator for (see
// ctConfig), and every tunnel's token is derived from the secret rather than
// carried, which is what keeps that answer inside one packet: a path that cuts
// a flow after a few packets still carries it.
//
// The body is base64url of: version (1), port (2, big endian), secret (12),
// then the host — 4 bytes for an IPv4 address, or the name as text.
const (
	connTestScheme   = shareScheme + "t."
	connTestSecret   = 12
	connTestLinkVer  = 3
	connTestFixedLen = 1 + 2 + connTestSecret
)

// Short is the link to copy.
func (l ConnTestLink) Short() string {
	secret, _ := base64.RawURLEncoding.DecodeString(l.Tok)
	b := []byte{connTestLinkVer, byte(l.Coord >> 8), byte(l.Coord)}
	b = append(b, secret...)
	if ip := net.ParseIP(l.Host).To4(); ip != nil {
		b = append(b, ip...)
	} else {
		host := l.Host
		// V1 reserves a four-byte tail for compact IPv4. Expand short IPv6
		// literals such as ::10 so old readers cannot mistake them for IPv4.
		if ip, err := netip.ParseAddr(host); err == nil && len(host) == 4 {
			host = ip.StringExpanded()
		}
		b = append(b, host...)
	}
	return connTestScheme + base64.RawURLEncoding.EncodeToString(b)
}

// connTestAddr is what a copied link says: where to ask, and the secret.
type connTestAddr struct {
	Host   string
	Coord  int
	Tok    string
	Routes []connTestAddr `json:"-"`
	Error  string         `json:"error,omitempty"`
}

// parseConnTestLink reads a copied test link, wherever it sits in what was
// pasted.
func parseConnTestLink(s string) (connTestAddr, error) {
	s = FindSetupLink(s)
	if !strings.HasPrefix(s, connTestScheme) {
		return connTestAddr{}, fmt.Errorf("that is not a Backpack connection-test link — it begins with %s", connTestScheme)
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, connTestScheme))
	if err != nil || len(b) <= connTestFixedLen || (b[0] != connTestLinkVer && b[0] != 4) {
		return connTestAddr{}, fmt.Errorf("the test link is damaged or uses an older coordinator; update both peers and create a new test")
	}
	if b[0] == 4 {
		return ctParseSuiteLink(b[1:])
	}
	a := connTestAddr{
		Coord: int(b[1])<<8 | int(b[2]),
		Tok:   base64.RawURLEncoding.EncodeToString(b[3:connTestFixedLen]),
	}
	host := b[connTestFixedLen:]
	if len(host) == 4 {
		a.Host = net.IP(host).String()
	} else {
		a.Host = string(host)
		if ip := net.ParseIP(a.Host); ip != nil {
			a.Host = ip.String()
		}
	}
	if a.Coord == 0 || a.Host == "" {
		return connTestAddr{}, fmt.Errorf("the test link is damaged or uses an older coordinator; update both peers and create a new test")
	}
	return a, nil
}

// IsConnTestLink reports whether s holds a connection-test link rather than a
// setup link.
func IsConnTestLink(s string) bool {
	_, err := parseConnTestLink(s)
	return err == nil
}

// ctNewSecret is a test's secret, as the link and the coordinator carry it.
func ctNewSecret() string {
	b := make([]byte, connTestSecret)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ctCaseToken is one tunnel's token, derived from the test's secret so that
// neither the link nor the coordinator's answer has to carry it.
func ctCaseToken(secret, kind, tr string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("backpack-conntest/" + kind + "/" + tr))
	if kind == "awg" {
		return ctAWGToken(m.Sum(nil))
	}
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// ctCompactLink removes local-only metadata and credentials derived by the peer.
func ctCompactLink(l ConnTestLink) ConnTestLink {
	l.V, l.Kind = 1, connTestKind
	// The copied link already supplies the authenticated coordinator endpoint.
	// fetchConnTestLink restores these fields before using any case. Repeating
	// its random secret and address consumes the one-packet settings budget.
	host := l.Host
	l.Host, l.Coord, l.Tok = "", 0, ""
	l.ID = "" // not used by the peer's engines or authentication
	l.Cases = append([]ShareLink(nil), l.Cases...)
	for i := range l.Cases {
		// Names are local artifact stems, never paired engine settings. Short,
		// unique names also remain usable by older connection-test workers.
		l.Cases[i].Name = "ct" + strconv.FormatInt(int64(i), 36)
		l.Cases[i].Tok = ""
		l.Cases[i].AWGPeer = "" // fetched as a separately authenticated bounded identity
		// Raw ICMP carriers ignore ports; only the token scopes their socket.
		// Do not spend packet space on random values with no wire meaning.
		if l.Cases[i].Kind == "direct" && (l.Cases[i].Tr == "xdi" || l.Cases[i].Tr == "icmp-request" || l.Cases[i].Tr == "icmp-reply") {
			l.Cases[i].Port = "1"
		}
		// Reverse forwarding listeners belong to Iran. The peer only dials
		// the tunnel endpoint; it never consumes these random port mappings.
		// Omitting them keeps the expanded carrier suite within one packet,
		// without changing either peer's transport, auth or tuning settings.
		if l.Cases[i].Kind == "reverse" {
			l.Cases[i].Ports = ""
		}
		if l.Cases[i].Host == host {
			l.Cases[i].Host = ""
		}
	}
	return l
}

func ctAdditionalCarrier(c ShareLink) bool {
	return c.Kind == "reverse" && (c.Tr == "tls" || c.Tr == "grpc" || c.Tr == "dns" || c.Tr == "ssh") || c.Kind == "direct" && (c.Tr == "dns" || c.Tr == "icmp-request" || c.Tr == "icmp-reply")
}

// New carriers travel separately, like managed helper credentials. Older workers
// keep their complete supported suite; updated workers fetch the added cases.
func ctConfigParts(l ConnTestLink) (main, additional string) {
	l = ctCompactLink(l)
	var base, extra []ShareLink
	for _, c := range l.Cases {
		if c.Kind == "kernel" {
			continue
		}
		if ctAdditionalCarrier(c) {
			extra = append(extra, c)
		} else {
			base = append(base, c)
		}
	}
	l.Cases, l.AdditionalDirect = base, len(extra)
	raw, _ := json.Marshal(l)
	main = gzipB64(raw)
	if len(extra) > 0 {
		raw, _ = json.Marshal(extra)
		additional = gzipB64(raw)
	}
	return
}

func ctConfig(l ConnTestLink) string { main, _ := ctConfigParts(l); return main }

// fetchConnTestLink asks the Iran side's coordinator for the test a copied
// link names, and fills in each tunnel's token.
func fetchConnTestLink(ctx context.Context, a connTestAddr) (ConnTestLink, error) {
	var out ConnTestLink
	for {
		reply, err := ctAskContext(ctx, a.Host, a.Coord, "config "+a.Tok)
		if err == nil && reply == "stopped" {
			return out, errConnTestStopped
		}
		if err == nil && strings.HasPrefix(reply, "config ") {
			raw, err := unGzipB64(strings.TrimPrefix(reply, "config "))
			out = ConnTestLink{}
			if err != nil || json.Unmarshal(raw, &out) != nil || out.Kind != connTestKind {
				return out, fmt.Errorf("the test settings from the Iran server arrived damaged — try again")
			}
			if out.AdditionalDirect != 0 {
				if out.AdditionalDirect < 0 || out.AdditionalDirect > 7 {
					return out, errors.New("invalid additional test settings count")
				}
				reply, askErr := ctAskContext(ctx, a.Host, a.Coord, "additional "+a.Tok)
				if askErr == nil && reply == "stopped" {
					return out, errConnTestStopped
				}
				if askErr != nil || !strings.HasPrefix(reply, "additional ") {
					if ctx.Err() != nil {
						return out, ctCoordinatorFailure(a.Host, a.Coord)
					}
					ctSleep(ctx, time.Second)
					continue
				}
				raw, decodeErr := unGzipB64(strings.TrimPrefix(reply, "additional "))
				var extra []ShareLink
				if decodeErr != nil || json.Unmarshal(raw, &extra) != nil || len(extra) != out.AdditionalDirect {
					return out, errors.New("additional test settings arrived damaged")
				}
				seen := make(map[string]bool)
				for _, c := range extra {
					key := c.Kind + "/" + c.Tr
					if !ctAdditionalCarrier(c) || seen[key] {
						return out, errors.New("invalid additional test carrier")
					}
					seen[key] = true
				}
				out.Cases = append(out.Cases, extra...)
			}
			out.Host, out.Coord, out.Tok = a.Host, a.Coord, a.Tok
			for i := range out.Cases {
				if out.Cases[i].Host == "" {
					out.Cases[i].Host = a.Host
				}
				out.Cases[i].Tok = ctCaseToken(a.Tok, out.Cases[i].Kind, ctLinkCaseKey(out, out.Cases[i]))
			}
			return out, nil
		}
		if ctx.Err() != nil {
			return out, ctCoordinatorFailure(a.Host, a.Coord)
		}
		ctSleep(ctx, 3*time.Second)
	}
}

// The coordinator uses ordinary TCP/UDP sockets. Failure to reach it cannot
// establish whether ICMP or the raw carriers pass the same path.
func ctCoordinatorFailure(host string, port int) error {
	return fmt.Errorf("the Iran server's test coordinator (%s port %d, TCP/UDP) did not answer. "+
		"Check the address, control port and that the Iran test is still open. "+
		"Tunnel protocols have not been measured; ICMP/raw carriers may still work. "+
		"On an ICMP-only path, try a Reverse or Direct xDi tunnel separately", host, port)
}

func gzipB64(raw []byte) string {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	return base64.RawURLEncoding.EncodeToString(buf.Bytes())
}

func unGzipB64(s string) ([]byte, error) {
	gz, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(io.LimitReader(zr, 256<<10))
}

// ConnTestResult is one tunnel's verdict.
type ConnTestResult struct {
	FreshOK        int     `json:"fresh_ok,omitempty"`
	FreshTried     int     `json:"fresh_tried,omitempty"`
	P95ms          float64 `json:"p95_ms,omitempty"`
	Observed       float64 `json:"observed,omitempty"`
	Mode           string  `json:"mode,omitempty"`
	CaseID         string  `json:"case,omitempty"`
	SNIDomain      string  `json:"sni,omitempty"`
	Kind           string  `json:"k"` // "reverse" or "direct"
	Transport      string  `json:"t"`
	Status         string  `json:"s"` // ok, unstable, down, skipped
	Detail         string  `json:"d,omitempty"`
	Connect        float64 `json:"c,omitempty"` // seconds to the first echo
	OK             int     `json:"o"`           // echoes that came back
	Tried          int     `json:"n"`           // echoes sent
	RTTms          int     `json:"r,omitempty"`
	JitterMs       float64 `json:"jitter_ms,omitempty"`
	JitterMeasured bool    `json:"jitter_measured,omitempty"`
	Mbps           float64 `json:"m,omitempty"`
	UploadMbps     float64 `json:"upload_mbps,omitempty"`
	SpeedStable    bool    `json:"speed_stable,omitempty"`
	Phase          string  `json:"phase,omitempty"`
	HTTPSProbe     string  `json:"https_probe,omitempty"` // independent TCP/TLS preflight, never a tunnel verdict
	TestPort       int     `json:"test_port,omitempty"`
	// Total is how many echoes the test sends each tunnel.
	Total int `json:"z,omitempty"`
}

// Result statuses.
const (
	ctTesting  = "testing" // still running
	ctOK       = "ok"
	ctUnstable = "unstable"
	ctDown     = "down"
	ctSkipped  = "skipped"
)

// connTestCase is one tunnel under test, as the Iran side holds it.
type connTestCase struct {
	mode       string
	kind, tr   string
	kernel     config.KernelConfig // completed local native settings for promotion
	key        string              // transport, or the domain-qualified SNI candidate identity
	name       string
	udp        bool   // the forwarded traffic is UDP (the udp transport)
	entry      int    // reverse: where on this server the tunnel's users arrive
	peerIP     string // direct: the kharej's tunnel address, where its echo listens
	l3         l3Spec // direct: this side's spec, finished once the kharej's address is known
	link       ShareLink
	skip       string
	httpsProbe string          // frozen from the authenticated coordinator before measurements start
	rtts       []time.Duration // every echo's round trip, for the recommendation
}

// ConnTestIran is a test the Iran side is running.
type ConnTestIran struct {
	helpersCancel context.CancelFunc
	dir           string
	link          ConnTestLink
	cases         []*connTestCase
	coord         *ctCoordinator
	mu            sync.Mutex
	engines       []*ctEngine
	root          bool
	best          ConnTestBest
	kernelFailure string
	realityTarget string
	setupCancel   context.CancelFunc
	realityLog    string
}

// ConnTestOptions are what the Iran operator answers.
type ConnTestOptions struct {
	Mode           string // quick: short traffic and speed samples for every healthy tunnel; full: longer samples
	skipKernel     bool   // focused fixtures that only exercise existing carriers
	skipAWG        bool   // focused integration fixtures; ordinary full tests include AWG
	AutomaticSetup bool   // keep endpoint-local identities for the post-test setup agent
	// A suite reserves ports across paths while child listeners are starting.
	ports     map[int]bool
	Context   context.Context // optional cancellation, including startup
	Host      string          // this server's address, as the kharej dials it
	SNIDomain string          // explicit single SNI; empty tests the built-in candidates
	// RealityTarget is an explicit reachable TLS 1.3/H2 hostname:port used
	// for the REALITY cover handshake. Empty selects a verified endpoint automatically.
	RealityTarget string
	// ShadowTLSCA optionally supplies public PEM trust for an explicit private
	// cover. Empty uses system trust; certificate verification is never disabled.
	ShadowTLSCA string
	// Direct includes the direct tunnels. They need root on both servers.
	Direct bool
	// Preset is balance, turbo or aggressive; empty is turbo, the wizards'
	// default.
	Preset string
	// SpoofSrc is the forged source the IP Spoofing Tester sends from, both
	// ways between the two servers (see ctSpoofProbe). Empty leaves it out.
	SpoofSrc string
}

// ConnTestSpoofSource is the forged source a Connection Test probes with.
const ConnTestSpoofSource = "10.10.10.10"

// The two directions the spoofing check runs, as its rows name them.
const (
	ctSpoofToKharej = "iran-kharej"
	ctSpoofToIran   = "kharej-iran"
)

// StartConnTestIran starts the Iran end of every tunnel under test and the
// coordinator, and returns the link to give the kharej.
func StartConnTestIran(o ConnTestOptions) (*ConnTestIran, string, error) {
	mode, modeErr := ConnTestMode(o.Mode)
	if modeErr != nil {
		return nil, "", modeErr
	}
	ctx := o.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	resolveCtx, cancelResolve := context.WithTimeout(ctx, connTestResolveWait)
	host, resolveErr := ctResolveHost(resolveCtx, o.Host, "")
	cancelResolve()
	if resolveErr != nil {
		return nil, "", resolveErr
	}
	if host == "" {
		return nil, "", fmt.Errorf("this server's address is needed: it is what the kharej dials")
	}
	dir, err := os.MkdirTemp("", "backpack-conntest-")
	if err != nil {
		return nil, "", err
	}
	helperCtx, helperCancel := context.WithCancel(ctx)
	s := &ConnTestIran{dir: dir, root: os.Geteuid() == 0, helpersCancel: helperCancel}
	fail := func(err error) (*ConnTestIran, string, error) {
		s.Close()
		return nil, "", err
	}

	preset := strings.ToLower(strings.TrimSpace(o.Preset))
	switch preset {
	case PresetBalance, PresetTurbo, PresetAggressive, PresetThroughput:
	default:
		preset = PresetTurbo
	}
	spoofSrc := strings.TrimSpace(o.SpoofSrc)
	if spoofSrc != "" {
		if ip := net.ParseIP(spoofSrc); ip == nil || ip.To4() == nil {
			return fail(fmt.Errorf("%q is not an IPv4 address to forge", spoofSrc))
		}
	}

	id := randomToken(8)
	used := o.ports
	if used == nil {
		used = map[int]bool{}
	}
	s.link = ConnTestLink{
		AutoSetup: o.AutomaticSetup,
		Mode:      mode,
		ID:        id, Host: host, Tok: ctNewSecret(), Preset: preset,
		TCP: ctPickPort(used, false), UDP: ctPickPort(used, false), L3: ctPickPort(used, false),
		Until:       time.Now().Add(connTestJoinWait + ConnTestRunTimeFor(mode) + connTestSlack).Unix(),
		DirectPorts: true,
		SNIVariants: o.Direct && strings.TrimSpace(o.SNIDomain) == "",
	}
	s.link.Coord = ctPickPort(used, true)
	if s.link.TCP == 0 || s.link.UDP == 0 || s.link.L3 == 0 || s.link.Coord == 0 {
		return fail(errors.New("no free ports for the connection test"))
	}
	if s.coord, err = startCTCoordinator(s.link.Coord, s.link.Tok); err != nil {
		return fail(fmt.Errorf("could not open the test coordinator on port %d: %w", s.link.Coord, err))
	}

	for _, tr := range connTestReverse {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		c := &connTestCase{kind: "reverse", tr: tr, name: "ct-" + id + "-" + tr, udp: tr == "udp"}
		if managedTransport(tr) {
			s.startManagedCase(c, used, o)
			s.cases = append(s.cases, c)
			continue
		}
		if connTestNeedsRoot(tr) && !s.root {
			c.skip = "needs root on both servers"
			s.cases = append(s.cases, c)
			continue
		}
		port := ctPickPort(used, true)
		c.entry = ctPickPort(used, true)
		if port == 0 || c.entry == 0 {
			return fail(fmt.Errorf("no free ports for the %s test", tr))
		}
		target := s.link.TCP
		if c.udp {
			target = s.link.UDP
		}
		spec := TunnelSpec{
			Role: "server", Transport: tr, Name: c.name,
			BindAddr: net.JoinHostPort(ctBindHost(host), strconv.Itoa(port)),
			Ports:    []string{fmt.Sprintf("127.0.0.1:%d=127.0.0.1:%d", c.entry, target)},
			Token:    ctCaseToken(s.link.Tok, "reverse", tr),
		}

		if tr == "ssh" {
			spec.BindAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			relay, relayErr := net.Listen("tcp", net.JoinHostPort(ctBindHost(host), "0"))
			if relayErr != nil {
				return fail(relayErr)
			}
			pin, relayErr := carrier.StartTestSSH(helperCtx, relay, spec.BindAddr, spec.Token)
			if relayErr != nil {
				relay.Close()
				return fail(relayErr)
			}
			spec.CarrierOptions = config.CarrierOptions{SSHUser: "backpack-test", SSHPort: relay.Addr().(*net.TCPAddr).Port, SSHHostKey: pin}
		}

		ApplyPreset(&spec, preset)
		raw := pendingReverseLink(spec, host, linkExtras{})
		if c.link, err = DecodeShareLink(raw); err != nil {
			return fail(fmt.Errorf("building the %s test link: %w", tr, err))
		}
		c.link.Name = c.name
		e, err := startCTEngine(dir, c.name, ctQuiet(spec.Render()))
		if err != nil {
			return fail(err)
		}
		s.engines = append(s.engines, e)
		s.cases = append(s.cases, c)
	}

	if o.Direct {
		for i, plan := range ctDirectPlans(o.SNIDomain) {
			carrier := plan.carrier
			if err := ctx.Err(); err != nil {
				return fail(err)
			}
			c := &connTestCase{kind: "direct", tr: carrier, key: plan.key, name: fmt.Sprintf("ct-%s-d%s-%d", id, carrier, i)}
			c.l3.SNIDomain = plan.sni
			if carrier == "spoof" && ctFamily(host) == "IPv6" {
				c.skip = "the forged-source carrier uses IPv4; IPv6 is not supported"
				s.cases = append(s.cases, c)
				continue
			}
			if !s.root {
				c.skip = "needs root on both servers"
				s.cases = append(s.cases, c)
				continue
			}
			block := ctFreeL3Block(i)
			c.peerIP = block + "2"
			c.l3 = l3Spec{
				Name: c.name, Side: sideIran, Carrier: carrier, Encap: "gre",
				// The kharej's address is not known until it joins; the link
				// carries only the port.
				Addr:  net.JoinHostPort("0.0.0.0", strconv.Itoa(ctPickPort(used, true))),
				Token: ctCaseToken(s.link.Tok, "direct", plan.key), Iface: fmt.Sprintf("bpt%d", i),
				SNIDomain: plan.sni,
				LocalIP:   block + "1/30", PeerIP: block + "2",
				// The wizard's defaults, the path MTU measured included: a
				// tunnel tested with other settings is not the one that
				// would be built.
				MTU: defaultL3MTU,
			}
			findL3Preset(preset).apply(&c.l3)
			if carrier == "connect-ip" {
				// Native RFC 9484 packets do not use Noise auto-MTU or GRE tuning.
				off := false
				c.l3.MTU, c.l3.AutoMTU = 1280, &off
			}
			if carrier == "spoof" {
				c.l3.Spoof.SpoofProfile = "udp"
			}
			raw := pendingShareLink(c.l3, linkExtras{})
			if c.link, err = DecodeShareLink(raw); err != nil {
				return fail(fmt.Errorf("building the %s test link: %w", carrier, err))
			}
			c.link.Name = c.name
			if carrier == "spoof" {
				c.link.Host = host
			}
			s.cases = append(s.cases, c)
		}
	}
	if o.Direct && !o.skipAWG {
		s.startAWGCase(id, used, true)
	}
	// IP spoofing, checked the way Manage → IP Spoofing Tester checks it: a
	// receiver on one server counts the probes that arrive from the forged
	// source, the other server sends them. Once each way.
	if spoofSrc != "" {
		if ctFamily(host) != "IPv6" {
			s.link.SpoofSrc, s.link.SpoofN = spoofSrc, (&connTestCase{mode: mode}).samples()
		}
		s.link.SpoofK = ctPickPort(used, false)
		s.link.SpoofI = ctPickPort(used, true)
		for _, dir := range []string{ctSpoofToKharej, ctSpoofToIran} {
			c := &connTestCase{kind: "spoof", tr: dir}
			if ctFamily(host) == "IPv6" {
				c.skip = "IP spoofing uses IPv4 packets; IPv6 is not supported"
			} else if !s.root {
				c.skip = "needs root on both servers"
			}
			s.cases = append(s.cases, c)
		}
	}

	s.addKernelCases(o)
	for _, c := range s.cases {
		c.mode = mode
		if c.skip == "" && c.kind != "spoof" {
			s.link.Cases = append(s.link.Cases, c.link)
		}
	}

	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	s.coord.setTestConfig(s.link)
	return s, s.link.Short(), nil
}

// ctCarrierSettings travels separately from the small configuration packet.
// Only public trust material is sent; credentials come from the test secret,
// and the certificate/private REALITY keys stay in the temporary Iran config.
type ctCarrierSettings struct {
	Tailcat    *ctTailcatPeer `json:"tc,omitempty"`
	TargetPort int            `json:"p"`
	ServerName string         `json:"s,omitempty"`
	CA         string         `json:"c,omitempty"`
	PublicKey  string         `json:"k,omitempty"`
	ShortID    string         `json:"i,omitempty"`
}

func ctUUID(token string) string {
	return token[:8] + "-" + token[8:12] + "-" + token[12:16] + "-" + token[16:20] + "-" + token[20:]
}

// ctCertificate creates a private test identity with explicit
// SANs. The peer verifies it with this leaf as its trust anchor, never insecure
// TLS. Unpromoted files are removed with the test directory. Its validity
// matches normal Backpack certificates so a promoted tunnel remains usable.
func ctCertificate(dir, name, host string) (cert, key, ca string, err error) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", "", err
	}
	leaf := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Minute),
		NotAfter: time.Now().AddDate(10, 0, 0), DNSNames: []string{"conntest.backpack.invalid"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if ip := net.ParseIP(host); ip != nil {
		leaf.IPAddresses = []net.IP{ip}
	} else {
		leaf.DNSNames = append(leaf.DNSNames, host)
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &private.PublicKey, private)
	if err != nil {
		return "", "", "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return "", "", "", err
	}
	ca = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	cert, key = filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	if err := os.WriteFile(cert, []byte(ca), 0600); err != nil {
		return "", "", "", err
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		return "", "", "", err
	}
	return cert, key, ca, nil
}

// Probe from Iran: DNS, certificate trust, TLS version and ALPN can differ by route.
func ctProbeRealityCover(ctx context.Context, target string, roots *x509.CertPool) error {
	return probeRealityTLS(ctx, target, "", roots)
}

func probeRealityTLS(ctx context.Context, target, serverName string, roots *x509.CertPool) error {
	d := tls.Dialer{NetDialer: &net.Dialer{}, Config: &tls.Config{
		ServerName: serverName, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}, RootCAs: roots,
		CurvePreferences: []tls.CurveID{tls.X25519},
	}}
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return err
	}
	defer conn.Close()
	state := conn.(*tls.Conn).ConnectionState()
	if state.Version != tls.VersionTLS13 || state.NegotiatedProtocol != "h2" {
		return errors.New("cover endpoint does not support TLS 1.3 and HTTP/2")
	}
	return nil
}

// A normal TLS handshake can pass while the pinned REALITY parser rejects the
// cover's ServerHello or certificate records. Authenticate through the actual
// Chrome/Vision helper pair before choosing a cover, so that case never gets
// reported as a blocked Iran/Kharej route merely because its cover is unusable.
func ctProbeRealityTransport(ctx context.Context, target, binary string) error {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return err
	}
	return probeRealityTransport(ctx, target, host, binary)
}

func probeRealityTransport(ctx context.Context, target, host, binary string) error {
	_, _, err := net.SplitHostPort(target)
	if err != nil {
		return err
	}
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer echo.Close()
	stopEcho := context.AfterFunc(ctx, func() { echo.Close() })
	defer stopEcho()
	done := make(chan struct{})
	go func() {
		defer close(done)
		peer, err := echo.Accept()
		if err != nil {
			return
		}
		defer peer.Close()
		stop := context.AfterFunc(ctx, func() { peer.Close() })
		defer stop()
		_, _ = io.Copy(peer, peer)
	}()
	// Join the echo worker even when helper startup or authentication fails.
	defer func() { echo.Close(); <-done }()
	used := map[int]bool{}
	port := ctPickPort(used, false)
	if port == 0 {
		return errors.New("no free port for the REALITY cover probe")
	}
	private, public, err := managedRealityKey("")
	if err != nil {
		return err
	}
	token := ctCaseToken(ctNewSecret(), "cover", "reality")
	server := config.ServerConfig{Transport: config.TCP, BindAddr: echo.Addr().String(),
		Xray: config.XrayServerConfig{Binary: binary, Mode: "reality", Listen: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
			UUID: ctUUID(token), ServerName: host, PrivateKey: private, ShortID: token[:16], Target: target}}
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	sh, err := naive.StartXrayServer(ctx, &server, logger)
	if err != nil {
		return fmt.Errorf("REALITY cover server: %w", err)
	}
	defer sh.Close()
	client := config.ClientConfig{Transport: config.TCP, RemoteAddr: server.BindAddr,
		Xray: config.XrayClientConfig{Binary: binary, Mode: "reality", Server: server.Xray.Listen,
			UUID: server.Xray.UUID, ServerName: host, PublicKey: public, ShortID: server.Xray.ShortID}}
	ch, err := naive.StartXrayClient(ctx, &client, logger)
	if err != nil {
		return fmt.Errorf("REALITY cover client: %w", err)
	}
	defer ch.Close()
	dialer, err := proxy.SOCKS5("tcp", strings.TrimPrefix(ch.ProxyURL(), "socks5://"), nil, &net.Dialer{})
	if err != nil {
		return err
	}
	conn, err := dialer.(proxy.ContextDialer).DialContext(ctx, "tcp", server.BindAddr)
	if err != nil {
		return fmt.Errorf("REALITY cover authentication: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	payload := []byte(token)
	if n, err := conn.Write(payload); err != nil {
		return err
	} else if n != len(payload) {
		return io.ErrShortWrite
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		return fmt.Errorf("REALITY cover did not carry authenticated data: %w", err)
	}
	if !bytes.Equal(got, payload) {
		return errors.New("REALITY cover probe returned corrupt data")
	}
	return ctx.Err()
}

var connTestCoverWait = 8 * time.Second

func ctFindRealityCover(ctx context.Context, targets []string, probe func(context.Context, string) error) (string, error) {
	var failures []string
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		attempt, cancel := context.WithTimeout(ctx, connTestCoverWait)
		err := probe(attempt, target)
		if err == nil {
			err = attempt.Err()
		}
		cancel()
		if err == nil && ctx.Err() == nil {
			return target, nil
		}
		failures = append(failures, target+": "+fmt.Sprint(err))
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no usable REALITY cover from Iran (%s)", strings.Join(failures, "; "))
}

func (s *ConnTestIran) startManagedCase(c *connTestCase, used map[int]bool, o ConnTestOptions) {
	tool := "xray"
	if c.tr == "naive" || config.SingProtocol(c.tr) {
		tool = "sing-box"
	}
	if c.tr == "tailcat" {
		tool = "tailcat"
	}
	var binary string
	var err error
	if c.tr != "short-https" && !config.ConnectProtocol(c.tr) {
		binary, err = connTestHelperBinary(tool)
		if err != nil {
			c.skip = err.Error()
			return
		}
	}
	coverHost := ""
	coverTarget := strings.TrimSpace(o.RealityTarget)
	if c.tr == "shadowtls" {
		if coverTarget == "" {
			coverTarget = "www.microsoft.com:443"
		}
		coverHost, _, err = net.SplitHostPort(coverTarget)
		if err != nil {
			c.skip = "invalid ShadowTLS cover"
			return
		}
	}
	if config.XrayREALITY(c.tr) {
		if coverTarget == "" {
			ctx := o.Context
			if ctx == nil {
				ctx = context.Background()
			}
			// The temporary loopback helpers emit banners and generic deployment
			// warnings. Keep them out of the menu without discarding diagnostics.
			s.realityLog = filepath.Join(s.dir, "reality-cover.log")
			output, openErr := os.OpenFile(s.realityLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if openErr != nil {
				c.skip = "creating REALITY probe diagnostics: " + openErr.Error()
				return
			}
			defer output.Close()
			ctx = naive.WithHelperOutput(ctx, output)
			coverTarget, err = ctFindRealityCover(ctx, []string{"www.microsoft.com:443", "www.bing.com:443", "www.apple.com:443"},
				func(ctx context.Context, target string) error {
					fmt.Fprintf(output, "\nREALITY cover probe: %s\n", target)
					if err := ctProbeRealityCover(ctx, target, nil); err != nil {
						return err
					}
					return ctProbeRealityTransport(ctx, target, binary)
				})
			if err != nil {
				c.skip = err.Error()
				return
			}
		}
		coverHost, _, err = net.SplitHostPort(coverTarget)
		if err != nil || coverHost == "" || net.ParseIP(coverHost) != nil {
			c.skip = "set a REALITY cover endpoint (reachable TLS 1.3/H2 hostname:port) on Iran"
			return
		}
	}
	port, targetPort := ctPickPort(used, false), ctPickPort(used, false)
	c.entry = ctPickPort(used, false)
	if port == 0 || targetPort == 0 || c.entry == 0 {
		c.skip = "no free ports for the managed test"
		return
	}
	token := ctCaseToken(s.link.Tok, c.kind, c.tr)
	spec := TunnelSpec{Role: "server", Transport: "tcp", Name: c.name, Token: token,
		BindAddr: net.JoinHostPort("127.0.0.1", strconv.Itoa(targetPort)),
		Ports:    []string{fmt.Sprintf("127.0.0.1:%d=127.0.0.1:%d", c.entry, s.link.TCP)}}
	ApplyPreset(&spec, s.link.Preset)
	// Reuse the existing reverse link's paired engine tuning; helper settings
	// are exchanged only through the authenticated test coordinator.
	c.link, err = DecodeShareLink(pendingReverseLink(spec, s.link.Host, linkExtras{}))
	if err != nil {
		c.skip = err.Error()
		return
	}
	c.link.Tr, c.link.Port, c.link.Name = c.tr, strconv.Itoa(port), c.name
	settings := ctCarrierSettings{TargetPort: targetPort, ServerName: "conntest.backpack.invalid"}
	if c.tr == "shadowtls" && o.ShadowTLSCA != "" {
		if _, err := managedLinkCertificates(o.ShadowTLSCA); err != nil {
			c.skip = "invalid ShadowTLS cover trust: " + err.Error()
			return
		}
		settings.CA = o.ShadowTLSCA
	}
	public := net.JoinHostPort(ctBindHost(s.link.Host), strconv.Itoa(port))
	var cert, key string
	if !config.XrayREALITY(c.tr) && c.tr != "shadowtls" && c.tr != "tailcat" {
		cert, key, settings.CA, err = ctCertificate(s.dir, c.name, s.link.Host)
		if err != nil {
			c.skip = "creating the test certificate: " + err.Error()
			return
		}
	}

	switch c.tr {
	case "tailcat":
		spec.SingServer = config.SingServerConfig{Binary: binary, Listen: public, Protocol: "tailcat"}
		if err := ctConfigureTailcatTest(&spec, &settings); err != nil {
			c.skip = err.Error()
			return
		}
	case "trusttunnel", "webtransport":
		spec.ConnectServer = config.ConnectServerConfig{Protocol: c.tr, Listen: public, ServerName: settings.ServerName, Username: "backpack", Password: token, Certificate: cert, Key: key, Mode: "auto"}
		if c.tr == "webtransport" {
			spec.ConnectServer.Mode = "h3"
			spec.ConnectServer.Path = "/" + token
		}
	case "short-https":
		settings.ServerName = s.link.Host
		spec.ShortServer = config.ShortServerConfig{Listen: public, ServerName: s.link.Host, Certificate: cert, Key: key}
	case "hysteria2", "anytls", "tuic", "shadowtls":
		spec.SingServer = config.SingServerConfig{Binary: binary, Listen: public, Protocol: c.tr, ServerName: settings.ServerName, Password: token, Certificate: cert, Key: key}
		if c.tr == "tuic" {
			spec.SingServer.UUID = ctUUID(token)
		}
		if c.tr == "hysteria2" {
			spec.SingServer.ObfsPassword = token
		}
		if c.tr == "shadowtls" {
			settings.ServerName = coverHost
			spec.SingServer.ServerName, spec.SingServer.Handshake = coverHost, coverTarget
			spec.SingServer.ShadowsocksKey = base64.StdEncoding.EncodeToString([]byte(token))
		}
	case "naive":
		spec.NaiveServer = config.NaiveServerConfig{Binary: binary, Listen: public,
			Username: "backpack", Password: token, Certificate: cert, Key: key}
	case "xhttp":
		spec.XrayServer = config.XrayServerConfig{Binary: binary, Listen: public, Mode: c.tr,
			UUID: ctUUID(token), ServerName: settings.ServerName, Path: "/" + token,
			Certificate: cert, Key: key}
	case "reality", "xhttp-reality":
		var private string
		private, settings.PublicKey, err = managedRealityKey("")
		if err != nil {
			c.skip = err.Error()
			return
		}
		settings.ServerName, settings.ShortID = coverHost, token[:16]
		spec.XrayServer = config.XrayServerConfig{Binary: binary, Listen: public, Mode: c.tr,
			UUID: ctUUID(token), ServerName: coverHost, PrivateKey: private, ShortID: settings.ShortID,
			Target: coverTarget}
		if config.XrayHTTP(c.tr) {
			spec.XrayServer.Path = "/" + token
		}
	}
	if err := validateManagedSpec(spec); err != nil {
		c.skip = "invalid helper settings: " + err.Error()
		return
	}
	engine, err := startCTEngine(s.dir, c.name, ctQuiet(spec.Render()))
	if err != nil {
		c.skip = "could not start here: " + err.Error()
		return
	}
	s.engines = append(s.engines, engine)
	if config.XrayREALITY(c.tr) {
		s.realityTarget = coverTarget
	}
	body, _ := json.Marshal(settings)
	s.coord.mu.Lock()
	if s.coord.carriers == nil {
		s.coord.carriers = make(map[string]string)
	}
	s.coord.carriers[c.tr] = gzipB64(body)
	s.coord.mu.Unlock()
}

func ctManagedClient(ctx context.Context, link ConnTestLink, c ShareLink, dir string) (TunnelSpec, error) {
	tool := "xray"
	if config.SingProtocol(c.Tr) {
		tool = "sing-box"
	}
	if c.Tr == "naive" {
		tool = "naive"
	}
	if c.Tr == "tailcat" {
		tool = "tailcat"
	}
	var binary string
	var err error
	if c.Tr != "short-https" && !config.ConnectProtocol(c.Tr) {
		binary, err = connTestHelperBinary(tool)
		if err != nil {
			return TunnelSpec{}, err
		}
	}
	reply, err := ctAskContext(ctx, link.Host, link.Coord, "carrier "+link.Tok+" "+c.Tr)
	if err != nil || !strings.HasPrefix(reply, "carrier ") {
		return TunnelSpec{}, fmt.Errorf("could not fetch %s helper settings: %v", c.Tr, err)
	}
	body, err := unGzipB64(strings.TrimPrefix(reply, "carrier "))
	var settings ctCarrierSettings
	if err != nil || json.Unmarshal(body, &settings) != nil || settings.TargetPort < 1 || settings.TargetPort > 65535 {
		return TunnelSpec{}, errors.New("invalid managed test settings")
	}
	// Use the same paired mapping as production Setup Links. Test trust is
	// written inside the disposable directory rather than the persistent store.
	paired := c
	paired.InnerPort = strconv.Itoa(settings.TargetPort)
	paired.HelperUser, paired.HelperPassword = "backpack", c.Tok
	paired.HelperUUID, paired.HelperSNI = ctUUID(c.Tok), settings.ServerName
	if config.SingProtocol(c.Tr) && c.Tr != "tuic" {
		paired.HelperUUID = ""
	}
	if c.Tr == "hysteria2" {
		paired.HelperObfs = c.Tok
	}
	if c.Tr == "shadowtls" {
		paired.HelperSSKey = base64.StdEncoding.EncodeToString([]byte(c.Tok))
	}
	paired.HelperPublicKey, paired.HelperShortID = settings.PublicKey, settings.ShortID
	if c.Tr == "tailcat" {
		if settings.Tailcat == nil {
			return TunnelSpec{}, fmt.Errorf("Tailcat peer identity unavailable")
		}
		paired.HelperTailcatKey, paired.HelperTailcatDisco, paired.HelperTailcatPSK = settings.Tailcat.Private, settings.Tailcat.Disco, settings.Tailcat.PSK
		paired.HelperDERPURL, paired.HelperDERPRegion = settings.Tailcat.Map, settings.Tailcat.Region
	}
	if config.XrayHTTP(c.Tr) || c.Tr == "webtransport" {
		paired.HelperPath = "/" + c.Tok
	}
	if c.Tr == "webtransport" {
		paired.HelperMode = "h3"
	} else if c.Tr == "trusttunnel" {
		paired.HelperMode = "auto"
	}
	spec := reverseClientFromLink(paired, link.Host)
	var ca string
	if !config.XrayREALITY(c.Tr) {
		if settings.CA == "" && c.Tr != "shadowtls" && c.Tr != "tailcat" {
			return TunnelSpec{}, errors.New("the managed TLS test has no trust certificate")
		}
		if settings.CA != "" {
			if _, err := managedLinkCertificates(settings.CA); err != nil {
				return TunnelSpec{}, err
			}
			ca = filepath.Join(dir, c.Tr+".crt")
			if err := os.WriteFile(ca, []byte(settings.CA), 0600); err != nil {
				return TunnelSpec{}, err
			}
		}
	}
	if config.ConnectProtocol(c.Tr) {
		spec.ConnectClient.CAFile = ca
	} else if c.Tr == "short-https" {
		spec.ShortClient.CAFile = ca
	} else if config.SingProtocol(c.Tr) {
		spec.SingClient.Binary, spec.SingClient.CAFile = binary, ca
	} else if c.Tr == "naive" {
		spec.NaiveClient.Binary, spec.NaiveClient.CAFile = binary, ca
	} else {
		spec.XrayClient.Binary, spec.XrayClient.CAFile = binary, ca
	}
	return spec, validateManagedSpec(spec)
}

// Joined is closed once the kharej has checked in.
func (s *ConnTestIran) Joined() <-chan struct{} { return s.coord.joined }

// Kharej is the address the kharej checked in from.
func (s *ConnTestIran) Kharej() string { return s.coord.peerAddr() }

// Run tests every tunnel once the kharej has joined, and publishes the
// verdict for the kharej to fetch. progress is told about each tunnel as it
// comes up or fails; it may be nil.
//
// Every tunnel is reported once at the start, skipped ones included, and each
// running one again after every echo, so a screen can show the test as it
// goes. progress is called from several goroutines at once.
func (s *ConnTestIran) Run(ctx context.Context, progress func(int, ConnTestResult)) []ConnTestResult {
	kharej := s.Kharej()
	s.coord.mu.Lock()
	for _, c := range s.cases {
		c.httpsProbe = s.coord.httpsProbes[c.tr]
		if c.skip == "" && s.coord.skipped[c.tr] {
			c.skip = "managed helper could not start on kharej; see its local diagnostic"
		}
		if c.skip == "" && (s.coord.skipped[c.kind+"/"+c.caseKey()] || s.coord.skipped[c.kind+"/"+c.tr]) {
			c.skip = "test could not start on Kharej; see its local diagnostic"
		}
		if c.skip == "" && ctSNIVariantKey(c.caseKey()) && s.coord.directPorts[c.caseKey()] == "" {
			c.skip = "automatic SNI candidates need an updated Kharej; update Backpack on both servers"
		}
	}
	s.coord.mu.Unlock()
	for _, c := range s.cases {
		if c.kind != "direct" || c.skip != "" {
			continue
		}
		_, port, _ := net.SplitHostPort(c.l3.Addr)
		port = s.coord.directPort(c.caseKey(), port)
		c.l3.Addr = net.JoinHostPort(kharej, port)
		if c.tr == "spoof" {
			c.l3.Spoof.SpoofPeerIP = kharej
		}

		if config.SingDatagram(c.tr) {
			s.coord.mu.Lock()
			raw := s.coord.carriers["direct/"+c.tr]
			s.coord.mu.Unlock()
			cfg, err := ctSingClient(raw, s.dir, c.name)
			if err != nil {
				c.skip = "native helper identity unavailable: " + err.Error()
				continue
			}
			c.l3.Sing = cfg
		}
		if c.tr == "masque" || c.tr == "masque-h2" || c.tr == "webtransport" || c.tr == "connect-ip" {
			s.coord.mu.Lock()
			raw := s.coord.masqueSettings
			if c.tr == "masque-h2" {
				raw = s.coord.masqueH2Settings
			}
			if c.tr == "webtransport" {
				raw = s.coord.webTransportSettings
			}
			if c.tr == "connect-ip" {
				raw = s.coord.connectIPSettings
			}
			s.coord.mu.Unlock()
			cfg, err := ctMasqueClient(raw, s.dir, c.name)
			if err != nil {
				c.skip = "MASQUE test identity unavailable: " + err.Error()
				continue
			}
			c.l3.Masque = cfg
		}
		_, body, err := directBodyFromSpec(c.l3)
		if err == nil {
			var e *ctEngine
			if e, err = startCTEngine(s.dir, c.name, body); err == nil {
				s.mu.Lock()
				s.engines = append(s.engines, e)
				s.mu.Unlock()
			}
		}
		if err != nil {
			c.skip = "could not start here: " + err.Error()
		}
	}

	results := make([]ConnTestResult, len(s.cases))
	var mu sync.Mutex
	reportRow := func(i int, r ConnTestResult) {
		r = ctHTTPSResult(r, s.cases[i])
		mu.Lock()
		results[i] = r
		mu.Unlock()
		if progress != nil {
			progress(i, r)
		}
	}
	for i, c := range s.cases {
		results[i] = c.initialResult()
		results[i] = ctHTTPSResult(results[i], c)
		if c.skip != "" {
			results[i].Status, results[i].Detail = ctSkipped, c.skip
		}
	}
	// The kharej sees the same rows, a question at a time.
	s.coord.setLive(func() []ConnTestResult {
		mu.Lock()
		defer mu.Unlock()
		return append([]ConnTestResult(nil), results...)
	})
	if progress != nil {
		for i, r := range results {
			progress(i, r)
		}
	}

	var wg sync.WaitGroup
	for i, c := range s.cases {
		if c.skip != "" || c.kind == "kernel" {
			continue
		}
		wg.Add(1)
		go func(i int, c *connTestCase) {
			defer wg.Done()
			report := func(r ConnTestResult) {
				reportRow(i, r)
			}
			if c.kind == "spoof" {
				s.spoofProbe(ctx, c, kharej, report)
				return
			}
			ctProbe(ctx, c, s.link.L3, report)
		}(i, c)
	}
	wg.Wait()
	// Stability probes finish before any bandwidth test starts. Heavy traffic
	// must not change another tunnel's loss/RTT or share its speed measurement.
	for i, c := range s.cases {
		r := results[i]
		if r.Phase != "speed-queued" {
			continue
		}
		if ctx.Err() != nil {
			r.Status, r.Detail, r.Phase = ctUnstable, "stopped before speed test", ""
			reportRow(i, r)
			continue
		}
		r.Phase, r.Detail = "speed", "measuring download and upload"
		reportRow(i, r)
		speed, err := ctSpeedForMode(ctx, func(dialCtx context.Context) (net.Conn, error) { return ctDial(dialCtx, c, s.link.L3) }, s.link.Mode)
		r.Phase, r.Detail = "", ""
		if err != nil {
			r.Status, r.Detail = ctUnstable, "speed test failed ("+ctShortErr(err)+")"
		} else {
			r.Status, r.Mbps, r.UploadMbps, r.SpeedStable = ctOK, speed.download, speed.upload, speed.stable
			if !speed.stable {
				r.Detail = "speed varied during the sample; repeat to confirm"
			}
		}
		reportRow(i, r)
	}

	// Native selectors and unkeyed protocols must never overlap test cases.
	for i, c := range s.cases {
		if c.kind != "kernel" || c.skip != "" {
			continue
		}
		s.runKernelCase(ctx, c, func(r ConnTestResult) { reportRow(i, r) })
	}

	// The kharej's path-MTU measurement, which it has usually sent long before.
	pmtu := 0
	select {
	case pmtu = <-s.coord.pmtu:
	case <-time.After(15 * time.Second):
	case <-ctx.Done():
	}
	s.best = ctComputeBest(results, s.cases, pmtu, s.dir)
	s.best.Host, s.best.Family = s.link.Host, ctFamily(s.link.Host)
	for i := range s.best.Recommendations {
		s.best.Recommendations[i].Host, s.best.Recommendations[i].Family = s.best.Host, s.best.Family
	}
	if s.best.Family == "IPv6" && s.best.MSS > 0 {
		s.best.MSS = pmtu - 60
	}
	s.coord.publish(results, s.best)
	return results
}

// Best is the recommendation the finished test came to.
func (s *ConnTestIran) Best() ConnTestBest { return s.best }

// spoofProbe is the IP Spoofing Tester, run in one direction with the one
// forged source, and judged the way the tester judges it: by how many of its
// probes arrived from that source. The Iran server sends and the kharej counts
// for one row, the other way round for the other; the kharej hands its count
// over through the coordinator.
func (s *ConnTestIran) spoofProbe(ctx context.Context, c *connTestCase, kharej string, report func(ConnTestResult)) {
	n := s.link.SpoofN
	r := ConnTestResult{Mode: s.link.Mode, Kind: c.kind, Transport: c.tr, Status: ctTesting, Total: n}
	report(r)
	forged := net.ParseIP(s.link.SpoofSrc)
	type count struct {
		arrived int
		err     error // nothing can be judged
		sendErr error // sending stopped part-way; what arrived still counts
	}
	finished := make(chan count, 1)
	start := time.Now()
	switch c.tr {
	case ctSpoofToKharej:
		go func() {
			// The kharej's receiver opens as it checks in; a moment's grace.
			ctSleep(ctx, 2*time.Second)
			// A send that fails part-way — a firewall on this server refusing
			// the rest — still leaves what went before it on the wire, so the
			// verdict is still what the kharej counted.
			sendErr := spooftest.RunSender(spooftest.SenderConfig{
				Context: ctx,
				Token:   s.link.Tok, TargetIP: net.ParseIP(kharej), DstPort: uint16(s.link.SpoofK),
				Attempts: n, Delay: time.Second, IPs: []net.IP{forged},
			})
			select {
			case got := <-s.coord.spoofArrived:
				if got == 0 && sendErr != nil {
					finished <- count{err: sendErr}
					return
				}
				finished <- count{arrived: got, sendErr: sendErr}
			case <-time.After(45 * time.Second):
				if sendErr == nil {
					sendErr = errors.New("the kharej never reported what arrived")
				}
				finished <- count{err: sendErr}
			case <-ctx.Done():
				finished <- count{err: ctx.Err()}
			}
		}()
	default: // ctSpoofToIran
		go func() {
			res, err := spooftest.RunReceiver(spooftest.ReceiverConfig{
				Context: ctx,
				Token:   s.link.Tok, Port: uint16(s.link.SpoofI), Attempts: n,
				Window: time.Duration(n)*time.Second + 20*time.Second,
			})
			finished <- count{arrived: ctArrivedFrom(res, forged), err: err}
		}()
	}

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			r.Status, r.Detail = ctUnstable, "stopped before the spoofing test completed"
			if r.OK == 0 {
				r.Status = ctDown
			}
			report(r)
			return
		case got := <-finished:
			r.Tried, r.OK = n, got.arrived
			switch {
			case got.err != nil:
				r.Status, r.OK, r.Detail = ctDown, 0, got.err.Error()
			case got.arrived == 0:
				r.Status = ctDown
				r.Detail = "no probe from " + s.link.SpoofSrc + " arrived — a private source is often dropped " +
					"on the way; a public one may pass (Manage → IP Spoofing Tester)"
				if c.tr == ctSpoofToIran {
					if why := ctStrictRPFilter(); why != "" {
						r.Detail = why
					}
				}
			case float64(n-got.arrived)*100/float64(n) <= 20: // the tester's default pass mark
				r.Status = ctOK
			default:
				r.Status = ctUnstable
			}
			if got.err == nil && got.sendErr != nil {
				r.Detail = "sending stopped part-way: " + got.sendErr.Error()
			}
			report(r)
			return
		case <-tick.C:
			// Probes go out one a second from the moment the far side starts.
			if sent := int(time.Since(start) / time.Second); sent <= n {
				r.Tried = sent
				report(r)
			}
		}
	}
}

// ctArrivedFrom is how many distinct probes arrived from ip.
func ctArrivedFrom(results []spooftest.Result, ip net.IP) int {
	for _, r := range results {
		if r.IP.Equal(ip) {
			return r.Arrived
		}
	}
	return 0
}

// ctKharejSpoof is the kharej's half of the spoofing check: it counts the Iran
// server's probes and reports the count, and sends its own.
func ctKharejSpoof(ctx context.Context, link ConnTestLink, root bool) {
	n := link.SpoofN
	forged := net.ParseIP(link.SpoofSrc)
	go func() {
		res, err := spooftest.RunReceiver(spooftest.ReceiverConfig{
			Context: ctx,
			Token:   link.Tok, Port: uint16(link.SpoofK), Attempts: n,
			Window: time.Duration(n)*time.Second + 20*time.Second,
		})
		got := 0
		if err == nil {
			got = ctArrivedFrom(res, forged)
		}
		for i := 0; i < 8 && ctx.Err() == nil; i++ {
			if reply, err := ctAskContext(ctx, link.Host, link.Coord, fmt.Sprintf("spoof %s %d", link.Tok, got)); err == nil && reply == "ok" {
				return
			}
			ctSleep(ctx, 2*time.Second)
		}
	}()
	if root {
		go func() {
			ctSleep(ctx, 2*time.Second)
			_ = spooftest.RunSender(spooftest.SenderConfig{
				Context: ctx,
				Token:   link.Tok, TargetIP: net.ParseIP(ctIPv4Context(ctx, link.Host)), DstPort: uint16(link.SpoofI),
				Attempts: n, Delay: time.Second, IPs: []net.IP{forged},
			})
		}()
	}
}

// Fetched is closed once the kharej has collected the verdict.
func (s *ConnTestIran) Fetched() <-chan struct{} { return s.coord.fetched }

// Close stops every engine and the coordinator and removes the test's files.
func (s *ConnTestIran) Close() {
	if s == nil {
		return
	}
	if s.helpersCancel != nil {
		s.helpersCancel()
	}
	s.mu.Lock()
	engines := s.engines
	s.engines = nil
	setupCancel := s.setupCancel
	s.mu.Unlock()
	if setupCancel != nil {
		setupCancel()
		s.coord.mu.Lock()
		setup := s.coord.setup
		s.coord.mu.Unlock()
		if setup != nil {
			<-setup.done
		}
	}
	ctStopAll(engines)
	if s.coord != nil {
		s.coord.close()
	}
	os.RemoveAll(s.dir)
}
