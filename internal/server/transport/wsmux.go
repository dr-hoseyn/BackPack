package transport

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/backpack/backpack/config" // for mode
	"github.com/backpack/backpack/internal/controlwire"
	"github.com/backpack/backpack/internal/metrics"
	"github.com/backpack/backpack/internal/traffic"
	"github.com/backpack/backpack/internal/utils/network"
	"github.com/xtaci/smux"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

type pendingWSMux struct {
	*websocket.Conn
	owner     context.Context
	version   int
	idleFloor int
	localIdle bool
	confirm   bool
	slots     chan struct{}
}

func (p *pendingWSMux) Close() error {
	err := p.Conn.Close()
	if p.slots != nil {
		<-p.slots
	}
	return err
}

// wsMuxGen is the state of a single run of the transport: the context that ends
// when the run does, and the channels its goroutines pass work over. Restart
// builds a fresh set for the next run, so carrying them here keeps a goroutine
// that outlives its run from reaching into the run that replaced it.
type wsMuxGen struct {
	idleTraffic       muxIdleTraffic
	drain             *wsMuxDrain
	queueTimeouts     atomic.Uint64
	openRetries       atomic.Uint64
	admissionRejected atomic.Uint64
	ctx               context.Context
	tunnelChannel     chan *pendingWSMux
	admissionSlots    chan struct{}
	localChannel      *muxLocalQueue
	reqNewConnChan    chan struct{}
	usageMonitor      *traffic.Usage
	// seat holds the client this generation serves, and serving starts what
	// outlives any one client — once, whichever claim comes first. See
	// clientSeat.
	seat    clientSeat
	serving sync.Once
}

type WsMuxTransport struct {
	// The generations of this transport and what outlives them: see
	// lifecycle.go.
	lifecycle

	config         *WsMuxConfig
	smuxConfig     *smux.Config
	controlChannel wsControl
	streamCounter  int32
	sessionCounter int32
	limits         *limiter
	tracePort      int
}

type WsMuxConfig struct {
	BindAddr         string
	Token            string
	SimpleAuth       bool
	SnifferLog       string
	TLSCertFile      string // Path to the TLS certificate file
	TLSKeyFile       string // Path to the TLS key file
	ACMEDomain       string // non-empty switches to Let's Encrypt for this domain
	ACMEEmail        string
	ACMECacheDir     string
	Ports            []string
	AcceptUDP        bool
	Nodelay          bool
	Sniffer          bool
	KeepAlive        time.Duration
	Heartbeat        time.Duration // in seconds
	ChannelSize      int
	MuxCon           int
	MaxSessions      int
	MuxVersion       int
	MaxFrameSize     int
	MaxReceiveBuffer int
	MaxStreamBuffer  int
	Mode             config.TransportType // ws or wss
	ProxyProtocol    bool
	// MSS caps the largest TCP segment the accepted tunnel connections send.
	// Zero leaves it to the kernel, which is the default; it is set where the
	// path silently drops full-sized packets. See manage.SetMSS.
	MSS int
	// MaxConnections caps simultaneous forwarded connections (0 = unlimited).
	MaxConnections int
	// BandwidthMbps caps total tunnel throughput (0 = unlimited).
	BandwidthMbps int
}

func NewWSMuxServer(parentCtx context.Context, cfg *WsMuxConfig, logger *logrus.Logger) *WsMuxTransport {
	// Initialize the TcpTransport struct
	server := &WsMuxTransport{
		smuxConfig: &smux.Config{
			Version:           network.ResolveStaticMuxVersion(cfg.MuxVersion),
			KeepAliveInterval: 20 * time.Second,
			KeepAliveTimeout:  40 * time.Second,
			MaxFrameSize:      network.WSMuxFrameSize(cfg.MaxFrameSize, cfg.Mode == config.WSSMUX),
			MaxReceiveBuffer:  cfg.MaxReceiveBuffer,
			MaxStreamBuffer:   cfg.MaxStreamBuffer,
		},
		config: cfg,
		lifecycle: lifecycle{
			parentctx: parentCtx,
			logger:    logger,
			usage:     usageSpec{snifferLog: cfg.SnifferLog, sniffer: cfg.Sniffer},
		},
		streamCounter:  0,
		sessionCounter: 0,
		limits:         newLimiter(Limits{MaxConnections: cfg.MaxConnections, BandwidthMbps: cfg.BandwidthMbps}),
	}
	if cfg.Mode == config.WSSMUX {
		if port, err := strconv.ParseUint(os.Getenv("BP_WSSMUX_TRACE_PORT"), 10, 16); err == nil {
			server.tracePort = int(port)
		}
	}

	// The first run is installed the same way every later one is, so there is
	// only one path that ever writes it.
	server.firstGeneration()

	return server
}

// Start brings up the first run. Every later one comes from Restart, which
// builds its own generation and hands it straight to start — so the fields read
// here are written once, by the constructor, before any other goroutine exists.
func (s *WsMuxTransport) Start() {
	s.start(s.newGen(s.run.context()))
}

// start runs one generation of the transport. Everything it needs is in g:
// nothing in here reaches back for a field that the next Restart is entitled to
// replace while this run is still using it.
func (s *WsMuxTransport) start(g *wsMuxGen) {
	go sweepTunnelConns(g.ctx, g.tunnelChannel)
	// for  webui

	s.status.set(fmt.Sprintf("Disconnected (%s)", s.config.Mode))

	go s.tunnelListener(g)

}

func (s *WsMuxTransport) Restart() {
	s.restart(s.controlChannel.Close, s.startAfterRestart)
}

func (s *WsMuxTransport) startAfterRestart(ctx context.Context) {
	s.controlChannel.Clear()
	s.counterMutex.Lock()
	atomic.StoreInt32(&s.streamCounter, 0)
	atomic.StoreInt32(&s.sessionCounter, 0)
	s.counterMutex.Unlock()
	go s.start(s.newGen(ctx))
}

// newGen builds one generation's channels and usage monitor.
func (s *WsMuxTransport) newGen(ctx context.Context) *wsMuxGen {
	queue := newBalancedMuxLocalQueue(s.config.ChannelSize)
	queue.retrySlowOpen = s.config.Mode == config.WSSMUX
	var drain *wsMuxDrain
	if s.config.Mode == config.WSSMUX {
		if path := os.Getenv("BP_WSSMUX_DRAIN_PEERS"); path != "" {
			drain = &wsMuxDrain{}
			drain.reload(path, s.logger)
			go drain.watch(ctx, path, s.logger)
		}
	}
	var slots chan struct{}
	if s.config.Mode == config.WSSMUX && s.config.MaxSessions > 0 {
		slots = make(chan struct{}, s.config.MaxSessions)
	}
	return &wsMuxGen{
		admissionSlots: slots,
		ctx:            ctx,
		tunnelChannel:  make(chan *pendingWSMux, s.config.ChannelSize),
		localChannel:   queue,
		drain:          drain,
		reqNewConnChan: make(chan struct{}, s.config.ChannelSize),
		usageMonitor:   s.usageMonitor(ctx),
	}
}

// control is this generation's control loop, bound to the control channel
// the generation was started with. See controlLoop.
func (s *WsMuxTransport) control(g *wsMuxGen, ctx context.Context, lost func()) controlLoop {
	return controlLoop{
		ctx:         ctx,
		link:        controlwire.WS(s.controlChannel.Get()),
		beat:        s.config.Heartbeat,
		readTimeout: 3 * maxLivenessBeat,
		requests:    g.reqNewConnChan,
		log:         s.logger,
		restart:     lost,
	}
}

// seatClient makes conn this generation's client, in place of whoever was.
func (s *WsMuxTransport) seatClient(g *wsMuxGen, conn *websocket.Conn) bool {
	accepted := g.seat.sit(g.ctx,
		func() { s.vacate(g) },
		func() {
			s.controlChannel.Set(conn)
			// See metrics.Snapshot.Connected: the watchdog asks the engine, not
			// the socket table.
			s.seated(conn.RemoteAddr().String())
			s.status.set(fmt.Sprintf("Connected (%s)", s.config.Mode))
			s.logger.Info("control channel established successfully")
		},
		func(ctx context.Context, lost func()) {
			loop := s.control(g, ctx, lost)
			// Bind to this claim even if its goroutine starts after replacement.
			loop.link = controlwire.WS(conn)
			loop.run()
		})
	if !accepted {
		// HTTP no longer owns a hijacked socket. Shutdown may have raced
		// the handler's earlier check, leaving no control loop to close it.
		conn.Close()
	}
	return accepted
}

// vacate empties the seat: the client's channel is closed and forgotten, and
// the mux sessions it opened that wait in the pool are dropped. The listener
// and the forwarded ports stay up for the next client.
func (s *WsMuxTransport) vacate(g *wsMuxGen) {
	s.controlChannel.Close()
	s.controlChannel.Clear()
	drainTunnelConns(g.tunnelChannel)
	s.status.set(fmt.Sprintf("Disconnected (%s)", s.config.Mode))
	metrics.ClearPeer()
}

func (s *WsMuxTransport) tunnelListener(g *wsMuxGen) {
	// Counted while this goroutine holds a listener, so Start can wait for the
	// port rather than sleeping and hoping. See listeners.go.
	if !s.listeners.hold(g.ctx) {
		return
	}
	defer s.listeners.release()

	addr := s.config.BindAddr
	upgrader := websocket.Upgrader{
		Subprotocols:     []string{network.WSMuxBinaryProtocol},
		ReadBufferSize:   wsReadBufferSize,
		WriteBufferSize:  wsWriteBufferSize,
		WriteBufferPool:  wsWriteBuffers,
		HandshakeTimeout: 45 * time.Second,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	// The decoy's identity is derived once here rather than per request: it is
	// fixed for the life of this server, and hashing the token on every probe
	// would be work an attacker could ask for.
	decoy := newDecoyProfile(s.config.Token)

	// Create an HTTP server
	server := &http.Server{
		Addr: addr,
		// Bound idle public HTTP keepalives. Hijacked WebSocket tunnels are
		// no longer managed by http.Server, so their lifetime is unchanged.
		IdleTimeout:       75 * time.Second,
		ReadHeaderTimeout: tunnelHeaderTimeout,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.logger.Tracef("received http request from %s", r.RemoteAddr)

			// Only a genuine tunnel connection (websocket upgrade, tunnel path,
			// valid credential) is served as a tunnel. Everything else — a
			// browser, a scanner, a probe with the wrong token — gets the decoy
			// website, so on 443 this looks like an ordinary HTTPS site rather
			// than a tunnel that answers with 401.
			if !isTunnelRequest(r, s.config.Token, s.config.SimpleAuth) {
				decoy.serve(w, r)
				return
			}

			response := serverProof(r, s.config.Token, s.config.SimpleAuth)
			if r.URL.Path == network.WSProbePath {
				serveWSProbe(w, r, &upgrader, response)
				return
			}
			var owner context.Context
			if r.URL.Path != "/channel" {
				owner = g.seat.client()
			}
			version, floor := s.smuxConfig.Version, 0
			localIdle := false
			if r.URL.Path != "/channel" {
				var err error
				version, floor, err = network.SelectWSMux(r.Header, s.config.MuxVersion)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if r.Header.Get(network.WSMuxOfferHeader) != "" {
					if response == nil {
						response = make(http.Header)
					}
					response.Set(network.WSMuxVersionHeader, strconv.Itoa(version))
					if floor > 0 {
						response.Set(network.WSMuxPoolHeader, strconv.Itoa(floor))
						localIdle = s.config.Mode == config.WSSMUX && r.Header.Get(network.WSMuxIdleHeader) == "1"
						if localIdle {
							response.Set(network.WSMuxIdleHeader, "1")
						}
					}
				}
			}
			confirm := s.config.Mode == config.WSSMUX &&
				r.Header.Get(network.WSMuxAdmissionHeader) == "1" && slices.Contains(websocket.Subprotocols(r), network.WSMuxBinaryProtocol)
			if confirm && owner == nil {
				g.admissionRejected.Add(1)
				http.Error(w, "websocket mux has no active control channel", http.StatusServiceUnavailable)
				return
			}
			var slots chan struct{}
			if r.URL.Path != "/channel" {
				slots = g.admissionSlots
				if slots != nil {
					select {
					case slots <- struct{}{}:
					default:
						g.admissionRejected.Add(1)
						http.Error(w, "websocket mux capacity exhausted", http.StatusServiceUnavailable)
						return
					}
					defer func() {
						if slots != nil {
							<-slots
						}
					}()
				}
			}
			if confirm {
				if response == nil {
					response = make(http.Header)
				}
				response.Set(network.WSMuxAdmissionHeader, "1")
			}
			conn, err := upgrader.Upgrade(network.WSMuxResponse(w), r, response)
			if err != nil {
				s.logger.Errorf("failed to upgrade connection from %s: %v", r.RemoteAddr, err)
				return
			}

			if r.URL.Path == "/channel" {
				if endedClaim(g.ctx, conn) {
					return
				}
				// A claim while a client is seated is that client re-dialing: it
				// takes the seat in place, and the ports and their users stay
				// up. It used to close both and rebuild the whole run. See
				// clientSeat.
				if g.seat.serving() {
					s.logger.Warn("a new control channel claim arrived; adopting the new client in place")
				}
				if !s.seatClient(g, conn) {
					return
				}
				g.serving.Do(func() { s.serveGeneration(s.forwarder(g), func() { s.handleLoop(g) }) })

			} else if strings.HasPrefix(r.URL.Path, "/tunnel") {
				accepted := g.seat.admitFor(g.ctx, owner, func(ctx context.Context) bool {
					entry := &pendingWSMux{Conn: conn, owner: ctx, version: version, idleFloor: floor,
						localIdle: localIdle, confirm: confirm, slots: slots}
					select {
					case g.tunnelChannel <- entry:
						slots = nil // transfer the reservation to the queued entry
						return true
					default:
						return false
					}
				})
				if !accepted {
					g.admissionRejected.Add(1)
					s.logger.Debugf("websocket mux pool is full or its client changed, closing connection from %s", conn.RemoteAddr())
					conn.Close()
				}
			}
		}),
	}

	// Built here rather than left to ListenAndServe, for the reason spelled out
	// in the ws transport: the socket that call opens carries none of the
	// tunnel's options, which is what made the MSS clamp a setting this
	// transport accepted and then ignored.
	// The tunnel's own port: retried rather than fatal. See bindfail.go.
	ln, ok := bindTunnelPort(g.ctx, s.logger, addr, func() (net.Listener, error) {
		// The websocket transports have never pinned the socket buffers.
		return network.ListenWithBuffers("tcp", addr, 0, 0, s.config.MSS, s.config.KeepAlive, !s.config.Nodelay)
	})
	if !ok {
		return
	}
	defer ln.Close()

	if s.config.Mode != config.WSMUX {
		// Fail configuration errors before serving. Retrying a missing PEM
		// file would hide the operator action required to repair this setup.
		tlsCfg, err := network.ServerTLSConfig(s.tlsSettings(), s.logger.Warnf)
		if err != nil {
			s.logger.Errorf("%s on %s cannot start: its TLS certificate could not be set up: %v", s.config.Mode, addr, err)
			return
		}
		server.TLSConfig = tlsCfg
	}
	s.serveWebSocketListener(g.ctx, server, ln, s.config.Mode, &s.controlChannel, s.startAfterRestart)
}

func (s *WsMuxTransport) handleLoop(g *wsMuxGen) {
	var report <-chan time.Time
	if s.config.Mode == config.WSSMUX {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		report = ticker.C
	}
	for {
		select {
		case <-g.ctx.Done():
			return

		case <-report:
			metrics.ReportWSMuxAdmission(metrics.WSMuxAdmissionStats{
				Carriers: int(atomic.LoadInt32(&s.sessionCounter)), QueueTimeouts: g.queueTimeouts.Load(),
				OpenRetries: g.openRetries.Load(), Rejected: g.admissionRejected.Load(),
				MaxCarriers: cap(g.admissionSlots), Reserved: len(g.admissionSlots),
			})
		case pending := <-g.tunnelChannel:
			go s.runCarrier(g, pending)
		}
	}
}

func (s *WsMuxTransport) runCarrier(g *wsMuxGen, pending *pendingWSMux) {
	defer pending.Close()
	stop := context.AfterFunc(pending.owner, func() { pending.Conn.Close() })
	defer stop()
	if pending.owner.Err() != nil {
		return
	}
	if pending.confirm {
		if err := network.WriteWSMuxAdmission(pending.Conn); err != nil {
			s.logger.Debugf("websocket mux admission confirmation failed: %v", err)
			return
		}
	}
	cfg := *s.smuxConfig
	cfg.Version = pending.version
	wire := network.WSMuxStream(pending.Conn)
	if s.config.Mode == config.WSSMUX {
		wire = network.WSMuxAnnouncementStream(pending.Conn)
	}
	session, err := network.NewWatchedMuxSession(wire, &cfg, smux.Client)
	if err != nil {
		s.logger.Errorf("failed to create MUX session for connection %s: %v", pending.RemoteAddr(), err)
		return
	}
	defer session.Close()
	if !s.countGeneration(g.ctx, &s.sessionCounter, 1) {
		return
	}
	go func() {
		// Only the reverse server opens data streams. A parser error or an
		// unexpected incoming stream ends this carrier.
		_, _ = session.AcceptStream()
		session.Close()
	}()

	loop := s.session(g, pending.owner)
	loop.pressure = network.WSMuxPressure(pending.Conn.NetConn())
	loop.bulk = network.WSMuxRecentBulk(wire)
	if g.drain != nil {
		s.logger.Debugf("WSSMux data carrier peer %s", pending.RemoteAddr())
		loop.draining = g.drain.forPeer(pending.RemoteAddr().String())
	}
	if s.tracePort != 0 {
		loop.traceCarrier = pending.Conn.NetConn()
	}
	if pending.idleFloor > 0 {
		loop.idleTimeout = 60 * time.Second
		loop.idleRetire = func() bool {
			if pending.localIdle {
				if g.localChannel.hasPending() {
					return false
				}
			} else {
				in, out := metrics.Traffic()
				if !g.idleTraffic.quiet(time.Now(), in, out) {
					return false
				}
			}
			return s.retireIdleSession(g.ctx, pending.owner, &s.sessionCounter, pending.idleFloor)
		}
	}
	loop.run(session)
}

// tlsSettings describes how this listener should obtain its certificate:
// Let's Encrypt when a domain is configured, otherwise the PEM pair on disk.
func (s *WsMuxTransport) tlsSettings() network.TLSSettings {
	return network.TLSSettings{
		CertFile:     s.config.TLSCertFile,
		KeyFile:      s.config.TLSKeyFile,
		ACMEDomain:   s.config.ACMEDomain,
		ACMEEmail:    s.config.ACMEEmail,
		ACMECacheDir: s.config.ACMECacheDir,
		// Only used when no certificate was configured at all, and cosmetic
		// even then — but a generated certificate that names the address it is
		// served from reads as a certificate rather than as a mistake.
		SelfSignedHost: certHost(s.config.BindAddr),
	}
}

// session binds this transport's channels, counters and settings to the shared
// loop. It is the whole of what is transport-specific about running a session.
// Its context is the seated client's, so the session ends with the client that
// opened it rather than with the generation — which now outlives its clients.
func (s *WsMuxTransport) session(g *wsMuxGen, owner context.Context) muxSession {
	frameSize := 0
	if s.config.Mode == config.WSSMUX {
		frameSize = s.smuxConfig.MaxFrameSize
	}
	return muxSession{
		relayFrame:    frameSize,
		tracePort:     s.tracePort,
		ctx:           owner,
		local:         g.localChannel,
		usage:         g.usageMonitor,
		reqNewConn:    g.reqNewConnChan,
		muxCon:        s.config.MuxCon,
		proxyProtocol: s.config.ProxyProtocol,
		sniffer:       s.config.Sniffer,
		limits:        s.limits,
		log:           s.logger,
		streams:       &s.streamCounter,
		sessions:      &s.sessionCounter,
		count: func(counter *int32, delta int32) {
			s.countGeneration(g.ctx, counter, delta)
		},
	}
}

// forwarder is this transport's forwarded ports for one generation.
func (s *WsMuxTransport) forwarder(g *wsMuxGen) portForwarder {
	f := portForwarder{
		expireQueued: s.session(g, g.ctx).dropLocal,
		ctx:          g.ctx, ports: s.config.Ports, acceptUDP: s.config.AcceptUDP,
		batchUDPReads: s.config.Mode == config.WSSMUX && s.config.BandwidthMbps <= 0,
		muxQueue:      g.localChannel, limits: s.limits, listeners: &s.listeners, log: s.logger,
		tune:         nodelayTune(s.config.Nodelay, s.logger),
		tcpKeepAlive: s.config.KeepAlive,
		queued:       muxRequestForGeneration(g.ctx, &s.counterMutex, &s.streamCounter, &s.sessionCounter, s.config.MuxCon, g.reqNewConnChan, s.logger),
	}
	if s.config.Mode == config.WSSMUX {
		drop := f.expireQueued
		f.expireQueued = func(local LocalTCPConn) {
			g.queueTimeouts.Add(1)
			drop(local)
		}
		f.onPairingRetry = func() { g.openRetries.Add(1) }
	}
	return f
}
