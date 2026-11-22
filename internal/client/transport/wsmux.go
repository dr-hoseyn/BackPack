package transport

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"slices"
	"strconv"
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

	config           *WsMuxConfig
	smuxConfig       *smux.Config
	sessions         muxPoolSessions
	poolLost         chan struct{}
	poolReady        chan struct{}
	dataRoutes       wsMuxDataRoutes
	tracePort        int
	confirmAdmission bool
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

func NewWSMuxClient(parentCtx context.Context, cfg *WsMuxConfig, logger *logrus.Logger) *WsMuxTransport {
	// Initialize the TcpTransport struct
	client := &WsMuxTransport{
		smuxConfig: &smux.Config{
			Version:           network.ResolveStaticMuxVersion(cfg.MuxVersion),
			KeepAliveInterval: 20 * time.Second,
			KeepAliveTimeout:  40 * time.Second,
			MaxFrameSize:      network.WSMuxFrameSize(cfg.MaxFrameSize, cfg.Mode == config.WSSMUX),
			MaxReceiveBuffer:  cfg.MaxReceiveBuffer,
			MaxStreamBuffer:   cfg.MaxStreamBuffer,
		},
		config:   cfg,
		poolLost: make(chan struct{}, 1),
	}

	if cfg.Mode == config.WSSMUX {
		client.poolReady = make(chan struct{}, 1)
		client.confirmAdmission = os.Getenv("BP_WSSMUX_CONFIRM_ADMISSION") == "1"
		if port, err := strconv.ParseUint(os.Getenv("BP_WSSMUX_TRACE_BACKEND_PORT"), 10, 16); err == nil {
			client.tracePort = int(port)
		}
	}

	client.firstGeneration(parentCtx, logger, usageSpec{snifferLog: cfg.SnifferLog, sniffer: cfg.Sniffer})
	return client
}

func (c *WsMuxTransport) Start() {

	c.status.set(fmt.Sprintf("Disconnected (%s)", c.config.Mode))

	c.state.Go(c.channelDialer)
}

func (c *WsMuxTransport) Restart() {
	c.restart(nil, func() {
		drain(c.poolLost)
		if c.poolReady != nil {
			drain(c.poolReady)
		}
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
	dial := func(ctx context.Context, addr string, attempts int) (*websocket.Conn, error) {
		return network.WebSocketDialer(ctx, cfg.Outbound, addr, cfg.EdgeIP, "/channel", cfg.DialTimeOut, cfg.KeepAlive, true, cfg.Token, cfg.Mode, cfg.SimpleAuth, attempts, 0, 0, cfg.MSS, cfg.WSS)
	}
	if cfg.Endpoints.Len() < 2 {
		return dial(ctx, cfg.Endpoints.Current(), 3)
	}
	if cfg.Mode == config.WSSMUX {
		timeout := cfg.DialTimeOut
		if timeout <= 0 {
			timeout = 45 * time.Second
		}
		budget, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		ctx = budget
	}

	// Try each route before starting a new failover round. A TCP
	// accept alone cannot identify a working TLS/WebSocket path, and racing
	// authenticated upgrades could claim the server's control seat twice.
	var lastErr error
	order := cfg.Endpoints.InPreferenceOrder()
	for index, addr := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attemptCtx := ctx
		cancel := func() {}
		if deadline, ok := ctx.Deadline(); ok && cfg.Mode == config.WSSMUX {
			attemptCtx, cancel = context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(order)-index))
		}
		conn, err := dial(attemptCtx, addr, 1)
		cancel()
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
	p := poolSizer{
		mux:          true,
		ctx:          c.state.Ctx(),
		log:          c.logger,
		size:         c.config.ConnPoolSize,
		aggressive:   c.config.AggressivePool,
		open:         &c.poolConnections,
		taken:        &c.loadConnections,
		shrink:       c.controlFlow,
		dial:         c.tunnelDialer,
		spawn:        c.state.Go,
		pending:      &c.dialingConnections,
		maxSize:      c.poolLimit(),
		sessions:     &c.sessions,
		sessionLost:  c.poolLost,
		sessionReady: c.poolReady,
		maxPending:   c.maxPendingDials(),
	}
	if c.config.Mode == config.WSSMUX {
		p.reportData = func(idle int) {
			metrics.ReportWSMuxData(metrics.WSMuxDataStats{
				Pending: int(atomic.LoadInt32(&c.dialingConnections)), Idle: idle,
				Routes: c.dataRoutes.snapshot(c.config.Endpoints.InPreferenceOrder()),
			})
		}
	}
	p.maintain()
}

// control is this generation's control loop. The websocket server listens
// for each heartbeat to be answered.
func (c *WsMuxTransport) control() controlLoop {
	loop := c.lifecycle.control(controlwire.WS(c.state.WSConn()), c.config.KeepAlive, c.tunnelDialer, c.Restart)
	loop.echoBeats = true
	return loop
}

func (c *WsMuxTransport) tunnelDialer() {
	ready, ok := c.beginPoolDial(c.poolLimit(), c.maxPendingDials())
	if !ok {
		return
	}
	defer ready()

	c.logger.Debugf("initiating new %s tunnel connection to address %s", c.config.Mode, c.config.RemoteAddr)

	mux := network.WSMuxOptions{
		Version: c.config.MuxVersion, PoolSize: c.config.ConnPoolSize, LocalIdle: c.config.Mode == config.WSSMUX, ConfirmAdmission: c.confirmAdmission,
	}
	tunnelWSConn, err := c.dialTunnel(c.state.Ctx(), &mux)
	if err != nil {
		if !errors.Is(err, network.ErrWSMuxCapacity) && !errors.Is(err, errWSMuxCooldown) {
			c.logger.Errorf("tunnel server dialer: %v", err)
		}
		return
	}

	defer c.state.Own(tunnelWSConn)()

	// Increment active connections counter
	atomic.AddInt32(&c.poolConnections, 1)
	ready()
	select {
	case c.poolReady <- struct{}{}:
	default:
	}

	c.handleSession(tunnelWSConn, mux)
}

// Limit concurrent TLS upgrades when a lost pool has a large target.
func (c *WsMuxTransport) maxPendingDials() int {
	if c.config.Mode == config.WSSMUX {
		return 16
	}
	return 0
}

func (c *WsMuxTransport) dialTunnel(ctx context.Context, mux *network.WSMuxOptions) (*websocket.Conn, error) {
	const maxAttempts = 3
	cfg := c.config
	dial := func(addr string, attempts int, timeout time.Duration) (*websocket.Conn, error) {
		options := *mux
		conn, err := network.WebSocketDialer(ctx, cfg.Outbound, addr, cfg.EdgeIP, "/tunnel", timeout, cfg.KeepAlive, cfg.Nodelay, cfg.Token, cfg.Mode, cfg.SimpleAuth, attempts, 2*1024*1024, 2*1024*1024, cfg.MSS, cfg.WSS, &options)
		if err == nil {
			*mux = options
		}
		return conn, err
	}
	first := cfg.Endpoints.Next()
	if cfg.Endpoints.Len() < 2 {
		started := time.Now()
		if cfg.Mode == config.WSSMUX && c.dataRoutes.atCapacity(first, started) {
			return nil, network.ErrWSMuxCapacity
		}
		conn, err := dial(first, maxAttempts, cfg.DialTimeOut)
		if cfg.Mode == config.WSSMUX && ctx.Err() == nil {
			c.dataRoutes.recordResult(first, started, err)
		}
		return conn, err
	}
	timeout := cfg.DialTimeOut
	if cfg.Mode == config.WSSMUX {
		// Explore alternatives quickly, then give a slow but viable route
		// its remaining budget if this round found no ready backup.
		round, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		ctx = round
		timeout = min(timeout, 800*time.Millisecond)
	}

	// Start with the preferred route, including ordinary backups whose
	// control and data upgrades may fail independently. Data dials never
	// move the control preference.
	var err error
	attempts := 0
	var slowRoute string
	quickAttempt := func(addr string) time.Duration {
		budget := c.dataRoutes.quickBudget(addr, timeout, cfg.DialTimeOut)
		if deadline, ok := ctx.Deadline(); ok && cfg.Mode == config.WSSMUX {
			remaining := min(cfg.Endpoints.Len(), maxAttempts) - attempts
			return min(budget, time.Until(deadline)/time.Duration(max(1, remaining)))
		}
		return budget
	}
	try := func(addr string, timeout time.Duration) *websocket.Conn {
		if timeout <= 0 && cfg.Mode == config.WSSMUX {
			err = context.DeadlineExceeded
			return nil
		}
		attempts++
		start := time.Now()
		conn, dialErr := dial(addr, 1, timeout)
		err = dialErr
		var timeoutErr net.Error
		if slowRoute == "" && errors.As(dialErr, &timeoutErr) && timeoutErr.Timeout() {
			slowRoute = addr
		}
		if ctx.Err() == nil {
			c.dataRoutes.recordResult(addr, start, dialErr)
		}
		if dialErr != nil {
			c.logger.Debugf("tunnel endpoint %s failed: %v", addr, dialErr)
		}
		return conn
	}
	if c.dataRoutes.available(first, time.Now()) {
		if conn := try(first, quickAttempt(first)); conn != nil {
			return conn, nil
		}
	}
	order := cfg.Endpoints.InPreferenceOrder()
	offset := 0
	if len(order) > maxAttempts && !cfg.Endpoints.Spread() && !cfg.Endpoints.DataHealthSteering() {
		// A short attempt budget must not permanently hide later backups
		// when the preferred route and the first alternatives fail slowly.
		order = slices.DeleteFunc(order, func(addr string) bool { return addr == first })
		offset = int((c.dataRoutes.fallbackCursor.Add(1) - 1) % uint64(len(order)))
	}
	for i := range order {
		addr := order[(i+offset)%len(order)]
		if attempts >= maxAttempts {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if addr == first || !c.dataRoutes.available(addr, time.Now()) {
			continue
		}
		if conn := try(addr, quickAttempt(addr)); conn != nil {
			return conn, nil
		}
	}
	// If every route is cooling down, probe one instead of waiting for the
	// entire cooldown to end. Next() still rotates that probe across routes.
	if attempts == 0 {
		if c.dataRoutes.atCapacity(first, time.Now()) {
			return nil, network.ErrWSMuxCapacity
		}
		if cfg.Mode == config.WSSMUX {
			if !c.dataRoutes.probing.CompareAndSwap(false, true) {
				return nil, errWSMuxCooldown
			}
			defer c.dataRoutes.probing.Store(false)
		}
		conn := try(first, quickAttempt(first))
		if conn != nil {
			return conn, nil
		}
	}
	if cfg.Mode == config.WSSMUX && cfg.DialTimeOut > timeout && ctx.Err() == nil && slowRoute != "" {
		conn := try(slowRoute, cfg.DialTimeOut)
		return conn, err
	}
	return nil, err
}

const wsMuxDataRetryDelay = 2 * time.Second

type wsMuxDataRoute struct {
	started       time.Time
	lastFailure   string
	lastFailureAt time.Time
	retryAt       time.Time
	attempts      uint64
	failures      uint64
	capacity      uint64
	fullUntil     time.Time
	handshake     time.Duration
}

type wsMuxDataRoutes struct {
	mu             sync.Mutex
	routes         map[string]wsMuxDataRoute
	fallbackCursor atomic.Uint64
	probing        atomic.Bool
}

func (r *wsMuxDataRoutes) available(addr string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.routes[addr]
	return !now.Before(state.retryAt) && !now.Before(state.fullUntil)
}

func (r *wsMuxDataRoutes) atCapacity(addr string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return now.Before(r.routes[addr].fullUntil)
}

func (r *wsMuxDataRoutes) quickBudget(addr string, fallback, limit time.Duration) time.Duration {
	r.mu.Lock()
	duration := r.routes[addr].handshake
	r.mu.Unlock()
	if duration == 0 || limit <= fallback {
		return fallback
	}
	return min(limit, max(fallback, min(3*time.Second, 2*duration)))
}

func (r *wsMuxDataRoutes) record(addr string, started time.Time, success bool) {
	var err error
	if !success {
		err = errWSMuxRoute
	}
	r.recordResult(addr, started, err)
}

var errWSMuxRoute = errors.New("websocket mux data route failed")
var errWSMuxCooldown = errors.New("websocket mux route recovery probe already running")

func (r *wsMuxDataRoutes) recordResult(addr string, started time.Time, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.routes == nil {
		r.routes = make(map[string]wsMuxDataRoute)
	}
	state := r.routes[addr]
	state.attempts++
	now := time.Now()
	full := errors.Is(err, network.ErrWSMuxCapacity)
	if full {
		state.capacity++
	} else if err != nil {
		state.failures++
	}
	if !started.Before(state.started) {
		state.started = started
		state.retryAt = time.Time{}
		state.fullUntil = time.Time{}
		if err != nil {
			state.lastFailureAt = now
			switch {
			case full:
				state.lastFailure = "BackPack admission limit"
			case errors.Is(err, network.ErrWSMuxUnavailable):
				state.lastFailure = "HTTP 503 from endpoint or proxy"
			case errors.Is(err, context.DeadlineExceeded):
				state.lastFailure = "dial deadline exceeded"
			default:
				state.lastFailure = "connection or upgrade failed"
			}
		}
		switch {
		case full:
			state.fullUntil = now.Add(wsMuxDataRetryDelay + rand.N(time.Second))
		case err != nil:
			state.retryAt = now.Add(wsMuxDataRetryDelay)
		default:
			elapsed := now.Sub(started)
			if state.handshake == 0 {
				state.handshake = elapsed
			} else {
				state.handshake = (3*state.handshake + elapsed) / 4
			}
		}
	}
	r.routes[addr] = state
}

func (r *wsMuxDataRoutes) snapshot(endpoints []string) []metrics.WSMuxRouteStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	routes := make([]metrics.WSMuxRouteStats, 0, len(r.routes))
	for _, addr := range endpoints {
		if route := r.routes[addr]; route.attempts != 0 {
			routes = append(routes, metrics.WSMuxRouteStats{
				Endpoint: addr, LastOK: route.retryAt.IsZero(), RetryAt: route.retryAt,
				Attempts: route.attempts, Failures: route.failures, CapacityRejected: route.capacity, CapacityRetryAt: route.fullUntil,
				LastFailure: route.lastFailure, LastFailureAt: route.lastFailureAt,
			})
		}
	}
	return routes
}

func (r *wsMuxDataRoutes) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.routes)
	r.fallbackCursor.Store(0)
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
	defer c.sessions.add(session, mux.Retirement, mux.LocalIdle)()
	backend := c.backend()
	if c.tracePort != 0 {
		backend.tracePort = c.tracePort
		backend.traceCarrier = tunnelConn.NetConn()
	}
	c.serveSession(session, tunnelConn.RemoteAddr(), backend)
}

// backend is how this transport's users reach the local service. See
// backend.go.
func (c *WsMuxTransport) backend() backendOpts {
	frameSize := 0
	if c.config.Mode == config.WSSMUX {
		frameSize = c.smuxConfig.MaxFrameSize
	}
	return backendOpts{
		relayFrame:  frameSize,
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
