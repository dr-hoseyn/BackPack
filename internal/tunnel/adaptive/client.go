//go:build !windows

package adaptive

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/metrics"
	"github.com/backpack/backpack/internal/nativeudp"
	"github.com/backpack/backpack/internal/pathprobe"
	"github.com/backpack/backpack/internal/tunnel/naive"
	"github.com/backpack/backpack/internal/utils"
	"github.com/backpack/backpack/internal/utils/network"
	"github.com/sirupsen/logrus"
)

type clientPath struct {
	name          string
	helper        *naive.Helper // guarded by ClientPaths.mu
	ready         atomic.Bool
	epoch         atomic.Uint64
	wake          chan struct{}
	probeSent     atomic.Uint64
	probeReceived atomic.Uint64
	probeAttempts atomic.Uint64
}
type proxyConnection struct {
	conn net.Conn
	path int
	idle bool
}

type ClientPaths struct {
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	once        sync.Once
	listener    net.Listener
	selector    *Selector
	policy      config.PathPolicy
	cfg         config.ClientConfig
	log         *logrus.Logger
	recordMu    sync.Mutex // serialize selection events and idle-pool retirement
	mu          sync.Mutex
	paths       []clientPath
	connections map[string]*proxyConnection
	packets     []*packetConn
	unregister  func()
}

func StartClient(parent context.Context, cfg *config.ClientConfig, log *logrus.Logger) (*ClientPaths, error) {
	if log == nil {
		log = logrus.StandardLogger()
	}
	if err := Validate(&config.Config{Client: *cfg}); err != nil {
		return nil, err
	}
	policy, _ := policyDefaults(cfg.PathPolicy)
	native := make([]bool, len(cfg.Paths))
	for i, p := range cfg.Paths {
		native[i] = p.Sing.NativeUDP
	}
	selector, err := NewSelector(policy, native)
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	c := &ClientPaths{ctx: ctx, cancel: cancel, listener: l, selector: selector, policy: policy, cfg: *cfg, log: log, paths: make([]clientPath, len(cfg.Paths)), connections: map[string]*proxyConnection{}}
	for i, p := range cfg.Paths {
		c.paths[i].name = p.Name
		c.paths[i].wake = make(chan struct{}, 1)
		child := clientConfig(*cfg, p)
		pathCtx := naive.WithReadiness(ctx, func(value bool) {
			c.mu.Lock()
			changed := c.paths[i].ready.Swap(value) != value
			if changed {
				c.paths[i].epoch.Add(1)
			}
			anyReady := false
			for j := range c.paths {
				anyReady = anyReady || c.paths[j].ready.Load()
			}
			c.mu.Unlock()
			if !value && changed {
				c.record(i, pathprobe.Result{}, nil)
			} else if changed {
				select {
				case c.paths[i].wake <- struct{}{}:
				default:
				}
			}
			naive.ReportReadiness(parent, anyReady)
		})
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			h, err := naive.StartWithRetry(pathCtx, log, func() (*naive.Helper, error) { return startClientHelper(pathCtx, &child, log) })
			if err != nil {
				return
			}
			defer h.Close()
			c.mu.Lock()
			c.paths[i].helper = h
			c.mu.Unlock()
			c.probeLoop(i)
		}()
	}
	c.wg.Add(1)
	go c.serve()
	c.unregister = metrics.RegisterManagedPaths(c.Status)
	return c, nil
}

func startClientHelper(ctx context.Context, cfg *config.ClientConfig, log *logrus.Logger) (*naive.Helper, error) {
	switch {
	case cfg.Short.Enabled():
		return naive.StartShortClient(ctx, cfg, log)
	case cfg.Sing.Enabled():
		return naive.StartSingClient(ctx, cfg, log)
	case cfg.Xray.Enabled():
		return naive.StartXrayClient(ctx, cfg, log)
	default:
		return naive.StartClient(ctx, cfg, log)
	}
}

func (c *ClientPaths) ProxyURL() string { return "socks5://" + c.listener.Addr().String() }

func (c *ClientPaths) Close() {
	c.once.Do(func() {
		c.cancel()
		c.listener.Close()
		c.mu.Lock()
		for _, p := range c.connections {
			p.conn.Close()
		}
		c.mu.Unlock()
	})
	c.wg.Wait()
	if c.unregister != nil {
		c.unregister()
	}
	c.mu.Lock()
	packets := append([]*packetConn(nil), c.packets...)
	c.mu.Unlock()
	for _, p := range packets {
		p.Close()
	}
}

func (c *ClientPaths) outbound(i int) (*network.Outbound, error) {
	c.mu.Lock()
	h := c.paths[i].helper
	c.mu.Unlock()
	if h == nil || !c.paths[i].ready.Load() {
		return nil, errors.New("local helper is not ready")
	}
	proxy, err := network.ParseProxy(h.ProxyURL())
	if err != nil {
		return nil, err
	}
	return &network.Outbound{Proxy: proxy}, nil
}

func (c *ClientPaths) dial(ctx context.Context, i int) (*net.TCPConn, error) {
	out, err := c.outbound(i)
	if err != nil {
		return nil, err
	}
	return network.TcpDialerVia(ctx, out, c.cfg.RemoteAddr, time.Duration(c.policy.ProbeTimeout)*time.Second, 30*time.Second, true, 1, 0, 0, 0)
}

func (c *ClientPaths) probe(i int) (pathprobe.Result, error) {
	c.paths[i].probeAttempts.Add(1)
	ctx, cancel := context.WithTimeout(c.ctx, time.Duration(c.policy.ProbeTimeout)*time.Second)
	defer cancel()
	started := time.Now()
	raw, err := c.dial(ctx, i)
	if err != nil {
		return pathprobe.Result{}, err
	}
	defer raw.Close()
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		raw.SetDeadline(deadline)
	}
	conn := naive.WrapDuplex(&probeCountingConn{Conn: raw, sent: &c.paths[i].probeSent, received: &c.paths[i].probeReceived})
	request, nonce, err := pathprobe.Request(c.cfg.Token, c.policy.ProbeBytes)
	if err != nil {
		return pathprobe.Result{}, err
	}
	if err := utils.SendBinaryTransportString(conn, request, utils.SG_PathProbe); err != nil {
		return pathprobe.Result{}, err
	}
	result, err := pathprobe.Check(conn, c.cfg.Token, nonce, started)
	if err != nil || !c.cfg.Paths[i].Sing.NativeUDP {
		return result, err
	}
	raw.Close()
	out, err := c.outbound(i)
	if err != nil {
		return result, err
	}
	pc, err := naive.NewSOCKSPacketConn(ctx, out.Proxy.Address, c.cfg.RemoteAddr)
	if err != nil {
		return result, err
	}
	defer pc.Close()
	peer, _ := net.ResolveUDPAddr("udp", c.cfg.RemoteAddr)
	udpStart := time.Now()
	if err := nativeudp.Probe(ctx, pc, peer, c.cfg.Token); err != nil {
		return result, err
	}
	if latency := time.Since(udpStart); latency > result.RTT {
		result.RTT = latency
	}
	return result, nil
}

func (c *ClientPaths) probeLoop(i int) {
	for c.ctx.Err() == nil {
		// Consume readiness already visible before this probe. A later helper
		// restart wakes the wait, rather than costing another probe interval.
		select {
		case <-c.paths[i].wake:
		default:
		}
		epoch := c.paths[i].epoch.Load()
		result, err := c.probe(i)
		c.recordProbe(i, epoch, result, err)
		t := time.NewTimer(c.selector.ProbeDelay(i))
		select {
		case <-c.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-c.paths[i].wake:
			t.Stop()
		}
	}
}

func (c *ClientPaths) record(i int, result pathprobe.Result, err error) {
	c.recordProbe(i, c.paths[i].epoch.Load(), result, err)
}

func (c *ClientPaths) recordProbe(i int, epoch uint64, result pathprobe.Result, err error) {
	c.recordMu.Lock()
	defer c.recordMu.Unlock()
	if epoch != c.paths[i].epoch.Load() {
		return // a response from the previous helper cannot revive its path
	}
	var changes []Switch
	if !c.paths[i].ready.Load() {
		changes = c.selector.Withdraw(i, time.Now())
	} else {
		changes = c.selector.Record(i, result, err, time.Now())
	}
	for _, change := range changes {
		// A backup promoted during its slow wait must be checked promptly.
		if change.New >= 0 {
			select {
			case c.paths[change.New].wake <- struct{}{}:
			default:
			}
		}
		name := func(i int) string {
			if i < 0 {
				return "none"
			}
			return c.paths[i].name
		}
		kind := "TCP"
		if change.Datagram {
			kind = "native UDP"
		} else {
			c.retireIdle(change.New)
		}
		c.log.Infof("verified managed %s path changed: %s -> %s", kind, name(change.Old), name(change.New))
	}
}

func (c *ClientPaths) Idle(conn *net.TCPConn) {
	c.mu.Lock()
	selected := c.selector.Pick(false)
	p := c.connections[conn.LocalAddr().String()]
	var closeConn net.Conn
	if p != nil {
		p.idle = true
		if p.path != selected {
			closeConn = p.conn
		}
	}
	c.mu.Unlock()
	if closeConn != nil {
		closeConn.Close()
	}
}
func (c *ClientPaths) Active(conn *net.TCPConn) {
	c.mu.Lock()
	if p := c.connections[conn.LocalAddr().String()]; p != nil {
		p.idle = false
	}
	c.mu.Unlock()
}
func (c *ClientPaths) retireIdle(selected int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.connections {
		if p.idle && p.path != selected {
			// Serialize closing against Active: an assignment cannot be
			// acknowledged between our idle check and the actual close.
			p.conn.Close()
		}
	}
}

func (c *ClientPaths) serve() {
	defer c.wg.Done()
	stop := context.AfterFunc(c.ctx, func() { c.listener.Close() })
	defer stop()
	slots := make(chan struct{}, 1024)
	for {
		conn, err := c.listener.Accept()
		if err != nil {
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		c.wg.Add(1)
		go func() { defer c.wg.Done(); defer func() { <-slots; conn.Close() }(); c.forward(conn) }()
	}
}

func (c *ClientPaths) forward(local net.Conn) {
	stop := context.AfterFunc(c.ctx, func() { local.Close() })
	defer stop()
	local.SetDeadline(time.Now().Add(5 * time.Second))
	if err := fixedSOCKS(local, c.cfg.RemoteAddr); err != nil {
		return
	}
	i := c.selector.Pick(false)
	if i < 0 {
		local.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, time.Duration(c.policy.ProbeTimeout)*time.Second)
	remote, err := c.dial(ctx, i)
	cancel()
	if err != nil {
		local.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
		c.record(i, pathprobe.Result{}, err)
		return
	}
	defer remote.Close()
	stopRemote := context.AfterFunc(c.ctx, func() { remote.Close() })
	defer stopRemote()
	key := local.RemoteAddr().String()
	c.mu.Lock()
	c.connections[key] = &proxyConnection{conn: local, path: i}
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.connections, key); c.mu.Unlock() }()
	if _, err := local.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	local.SetDeadline(time.Time{})
	remote.SetDeadline(time.Time{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := io.Copy(remote, local); err != nil {
			local.Close()
			remote.Close()
			return
		}
		remote.CloseWrite()
	}()
	_, downErr := io.Copy(local, remote)
	if tcp, ok := local.(*net.TCPConn); ok {
		tcp.CloseWrite()
	}
	// A clean EOF is directional. Only an error tears down both directions.
	if downErr != nil {
		local.Close()
		remote.Close()
	}
	<-done
}
