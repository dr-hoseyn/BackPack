package transport

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/controlwire"
	"github.com/backpack/backpack/internal/metrics"
	"github.com/backpack/backpack/internal/utils/network"
	"github.com/xtaci/smux"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

type WsMuxTransport struct {
	// The generations of this transport and what outlives them: see
	// lifecycle.go.
	lifecycle

	config     *WsMuxConfig
	smuxConfig *smux.Config
	sessions   muxPoolSessions
	poolLost   chan struct{}
	dataRoutes wsMuxDataRoutes
}
type WsMuxConfig struct {
	RemoteAddr string
	// Endpoints rotates through the server addresses (primary + fallbacks)
	// so a filtered IP or blocked port does not stop the tunnel.
	Endpoints        *network.Endpoints
	Token            string
	SnifferLog       string
	Nodelay          bool
	Sniffer          bool
	KeepAlive        time.Duration
	RetryInterval    time.Duration
	DialTimeOut      time.Duration
	MuxVersion       int
	MaxFrameSize     int
	MaxReceiveBuffer int
	MaxStreamBuffer  int
	ConnPoolSize     int
	MaxSessions      int
	Mode             config.TransportType
	SimpleAuth       bool
	WSS              *network.WSSClientOptions
	AggressivePool   bool
	EdgeIP           string
	// MSS caps the largest TCP segment these connections send. Zero leaves it
	// to the kernel, which is the default and almost always right; it is set
	// where the path silently drops full-sized packets. See manage.SetMSS.
	MSS int
	// Outbound says how the connections that reach the tunnel server leave
	// this machine: through a proxy, from a chosen source address or
	// interface, under a routing mark. Nil dials directly. None of it is ever
	// applied to the dial to the local backend — see network/outbound.go.
	Outbound *network.Outbound
}

func NewWSMuxClient(parentCtx context.Context, config *WsMuxConfig, logger *logrus.Logger) *WsMuxTransport {
	// Initialize the TcpTransport struct
	client := &WsMuxTransport{
		smuxConfig: &smux.Config{
			Version:           network.ResolveStaticMuxVersion(config.MuxVersion),
			KeepAliveInterval: 20 * time.Second,
			KeepAliveTimeout:  40 * time.Second,
			MaxFrameSize:      config.MaxFrameSize,
			MaxReceiveBuffer:  config.MaxReceiveBuffer,
			MaxStreamBuffer:   config.MaxStreamBuffer,
		},
		config:   config,
		poolLost: make(chan struct{}, 1),
	}

	client.firstGeneration(parentCtx, logger, usageSpec{snifferLog: config.SnifferLog, sniffer: config.Sniffer})
	return client
}

func (c *WsMuxTransport) Start() {

	c.status.set(fmt.Sprintf("Disconnected (%s)", c.config.Mode))

	c.state.Go(c.channelDialer)
}

func (c *WsMuxTransport) Restart() {
	c.restart(nil, func() {
		drain(c.poolLost)
		c.dataRoutes.reset()
	}, c.Start)
}

func (c *WsMuxTransport) channelDialer() {
	c.logger.Infof("attempting to establish a new %s control channel connection", c.config.Mode)

	// One backoff for this reconnect loop (see backoff.go): fixed-interval
	// retries become exponential, so a sustained outage is probed a few times a
	// minute rather than every second.
	bo := newBackoff(c.config.RetryInterval)

	for {
		select {
		case <-c.state.Ctx().Done():
			return
		default:

			tunnelWSConn, err := c.dialControl(c.state.Ctx())
			if err != nil {
				c.logger.Errorf("control channel dialer: %v", err)
				bo.Wait(c.state.Ctx())
				continue
			}
			// See metrics.Snapshot.Connected: the watchdog asks the engine, not the
			// socket table.
			metrics.ReportPeer(tunnelWSConn.RemoteAddr().String())
			c.state.SetWSConn(tunnelWSConn)
			c.logger.Info("control channel established successfully")

			c.status.set(fmt.Sprintf("Connected (%s)", c.config.Mode))

			c.state.Go(c.poolMaintainer)
			loop := c.control()
			c.state.Go(func() { loop.run() })

			return
		}
	}
}

func (c *WsMuxTransport) dialControl(ctx context.Context) (*websocket.Conn, error) {
	cfg := c.config
	dial := func(addr string, attempts int) (*websocket.Conn, error) {
		return network.WebSocketDialer(ctx, cfg.Outbound, addr, cfg.EdgeIP, "/channel", cfg.DialTimeOut, cfg.KeepAlive, true, cfg.Token, cfg.Mode, cfg.SimpleAuth, attempts, 0, 0, cfg.MSS, cfg.WSS)
	}
	if cfg.Endpoints.Len() < 2 {
		return dial(cfg.Endpoints.Current(), 3)
	}

	// Give each route one full handshake before retrying any route. A TCP
	// accept alone cannot identify a working TLS/WebSocket path, and racing
	// authenticated upgrades could claim the server's control seat twice.
	var lastErr error
	for _, addr := range cfg.Endpoints.InPreferenceOrder() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := dial(addr, 1)
		if err == nil {
			cfg.Endpoints.Prefer(addr)
			return conn, nil
		}
		lastErr = fmt.Errorf("%s: %w", addr, err)
		c.logger.Debugf("control endpoint failed: %v", lastErr)
	}
	return nil, lastErr
}

// poolMaintainer keeps the pool the right size. The policy is poolSizer's,
// shared with every other client transport — see poolmaintain.go.
func (c *WsMuxTransport) poolMaintainer() {
	poolSizer{
		mux:         true,
		ctx:         c.state.Ctx(),
		log:         c.logger,
		size:        c.config.ConnPoolSize,
		aggressive:  c.config.AggressivePool,
		open:        &c.poolConnections,
		taken:       &c.loadConnections,
		shrink:      c.controlFlow,
		dial:        c.tunnelDialer,
		spawn:       c.state.Go,
		pending:     &c.dialingConnections,
		maxSize:     c.poolLimit(),
		sessions:    &c.sessions,
		sessionLost: c.poolLost,
	}.maintain()
}

// control is this generation's control loop. The websocket server listens
// for each heartbeat to be answered.
func (c *WsMuxTransport) control() controlLoop {
	loop := c.lifecycle.control(controlwire.WS(c.state.WSConn()), c.config.KeepAlive, c.tunnelDialer, c.Restart)
	loop.echoBeats = true
	return loop
}

func (c *WsMuxTransport) tunnelDialer() {
	ready, ok := c.beginPoolDial(c.poolLimit())
	if !ok {
		return
	}
	defer ready()

	c.logger.Debugf("initiating new %s tunnel connection to address %s", c.config.Mode, c.config.RemoteAddr)

	mux := network.WSMuxOptions{Version: c.config.MuxVersion, PoolSize: c.config.ConnPoolSize}
	tunnelWSConn, err := c.dialTunnel(c.state.Ctx(), &mux)
	if err != nil {
		c.logger.Errorf("tunnel server dialer: %v", err)

		return
	}

	defer c.state.Own(tunnelWSConn)()

	// Increment active connections counter
	atomic.AddInt32(&c.poolConnections, 1)
	ready()

	c.handleSession(tunnelWSConn, mux)
}

func (c *WsMuxTransport) dialTunnel(ctx context.Context, mux *network.WSMuxOptions) (*websocket.Conn, error) {
	const maxAttempts = 3
	cfg := c.config
	dial := func(addr string, attempts int) (*websocket.Conn, error) {
		return network.WebSocketDialer(ctx, cfg.Outbound, addr, cfg.EdgeIP, "/tunnel", cfg.DialTimeOut, cfg.KeepAlive, cfg.Nodelay, cfg.Token, cfg.Mode, cfg.SimpleAuth, attempts, 2*1024*1024, 2*1024*1024, cfg.MSS, cfg.WSS, mux)
	}
	first := cfg.Endpoints.Next()
	if cfg.Endpoints.Len() < 2 || (!cfg.Endpoints.Spread() && !cfg.Endpoints.DataHealthSteering()) {
		return dial(first, maxAttempts)
	}

	// Start with the spread or health preference. If that route is down,
	// try the control route and other candidates within the same attempt
	// count. Data dials never move the control preference.
	var err error
	attempts := 0
	try := func(addr string) *websocket.Conn {
		attempts++
		start := time.Now()
		conn, dialErr := dial(addr, 1)
		err = dialErr
		if ctx.Err() == nil {
			c.dataRoutes.record(addr, start, dialErr == nil)
		}
		if dialErr != nil {
			c.logger.Debugf("tunnel endpoint %s failed: %v", addr, dialErr)
		}
		return conn
	}
	if c.dataRoutes.available(first, time.Now()) {
		if conn := try(first); conn != nil {
			return conn, nil
		}
	}
	for _, addr := range cfg.Endpoints.InPreferenceOrder() {
		if attempts >= maxAttempts {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if addr == first || !c.dataRoutes.available(addr, time.Now()) {
			continue
		}
		if conn := try(addr); conn != nil {
			return conn, nil
		}
	}
	// If every route is cooling down, probe one instead of waiting for the
	// entire cooldown to end. Next() still rotates that probe across routes.
	if attempts == 0 {
		conn := try(first)
		return conn, err
	}
	return nil, err
}

const wsMuxDataRetryDelay = 2 * time.Second

type wsMuxDataRoute struct {
	started time.Time
	retryAt time.Time
}

type wsMuxDataRoutes struct {
	mu     sync.Mutex
	routes map[string]wsMuxDataRoute
}

func (r *wsMuxDataRoutes) available(addr string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !now.Before(r.routes[addr].retryAt)
}

func (r *wsMuxDataRoutes) record(addr string, started time.Time, success bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if started.Before(r.routes[addr].started) {
		return // a newer attempt already reported this route
	}
	if r.routes == nil {
		r.routes = make(map[string]wsMuxDataRoute)
	}
	state := wsMuxDataRoute{started: started}
	if !success {
		state.retryAt = time.Now().Add(wsMuxDataRetryDelay)
	}
	r.routes[addr] = state
}

func (r *wsMuxDataRoutes) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.routes)
}

func (c *WsMuxTransport) handleSession(tunnelConn *websocket.Conn, mux network.WSMuxOptions) {
	established := false
	defer func() {
		atomic.AddInt32(&c.poolConnections, -1)
		if established && c.state.Ctx().Err() == nil {
			select {
			case c.poolLost <- struct{}{}:
			default:
			}
		}
	}()

	// SMUX server
	cfg := *c.smuxConfig
	cfg.Version = mux.NegotiatedVersion
	session, err := network.NewWatchedMuxSession(network.WSMuxStream(tunnelConn), &cfg, smux.Server)
	if err != nil {
		c.logger.Errorf("failed to create mux session: %v", err)
		return
	}

	established = true
	defer c.sessions.add(session, mux.Retirement)()
	c.serveSession(session, tunnelConn.RemoteAddr(), c.backend())
}

// backend is how this transport's users reach the local service. See
// backend.go.
func (c *WsMuxTransport) backend() backendOpts {
	return backendOpts{
		dialTimeout: c.config.DialTimeOut,
		keepAlive:   c.config.KeepAlive,
		rcvBuf:      0,
		sndBuf:      0,
		mss:         0,
		sniffer:     c.config.Sniffer,
	}
}

// An explicit operational ceiling is independent of lazy receive windows:
// limiting all peers by window capacity would strand large numbers of idle
// users even when CPU and resident memory remain available.
func (c *WsMuxTransport) poolLimit() int {
	if c.config.MaxSessions > 0 {
		return max(c.config.ConnPoolSize, c.config.MaxSessions)
	}
	return muxPoolLimit(c.config.ConnPoolSize)
}
