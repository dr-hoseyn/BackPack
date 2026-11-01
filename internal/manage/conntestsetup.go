package manage

// A short-lived, authenticated setup session on the test coordinator. Either
// operator can choose the tested tunnel; only the Iran side assigns the pair's
// fresh credentials and settings. The kharej polls, so no inbound management
// port, SSH credential or second copied link is needed.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/backpack/backpack/internal/app"
	"github.com/backpack/backpack/internal/manage/core"
)

const ConnTestSetupWindow = 15 * time.Minute

type ConnTestSetupRequest struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Transport string `json:"transport"`
	Ports     string `json:"ports"`
	AcceptUDP bool   `json:"acceptUDP"`
	SNIDomain string `json:"sni,omitempty"`
}

// Every tested engine can promote its endpoint-local identities and trust.
func ctSetupSupported(kind, tr string) bool {
	if kind == "reverse" && tr == "ssh" {
		return false
	}
	if kind == "kernel" {
		_, _, ok := ctKernelMethod(tr)
		return ok
	}
	if kind == "reverse" {
		return validTransport(tr) || managedTransport(tr)
	}
	if kind == "awg" {
		return tr == "awg"
	}
	if kind == "direct" {
		for _, c := range connTestDirect {
			if c == tr {
				return true
			}
		}
	}
	return false
}

type ConnTestSetupChoice struct {
	Kind      string  `json:"kind"`
	Transport string  `json:"transport"`
	SNIDomain string  `json:"sni,omitempty"`
	Mbps      float64 `json:"mbps"`
}

// ConnTestSetupView deliberately contains no tunnel credentials.
type ConnTestSetupView struct {
	State   string                `json:"state"` // waiting, preparing, applying, verifying, done, failed, expired
	ID      string                `json:"id,omitempty"`
	Error   string                `json:"error,omitempty"`
	Iran    string                `json:"iran,omitempty"`
	Kharej  string                `json:"kharej,omitempty"`
	Ready   bool                  `json:"ready"`
	Expires int64                 `json:"expires"`
	Choices []ConnTestSetupChoice `json:"choices,omitempty"`
}

type ctSetupWire struct {
	View ConnTestSetupView `json:"view"`
	Form *PeerForm         `json:"form,omitempty"`
}

type ctSetupAck struct {
	ID    string `json:"id"`
	Stage string `json:"stage"`
	Error string `json:"error,omitempty"`
}

type ctSetupOps struct {
	prepare  func(PeerForm) (PeerForm, error)
	validate func(PeerForm) error
	apply    func(PeerForm) (string, bool, error)
	remove   func(PeerForm) error
	verify   func(context.Context, string) error
}

func ctDefaultSetupOps() ctSetupOps {
	return ctSetupOps{validate: ctValidateSetupForm, apply: ctApplySetupForm, remove: ctRemoveSetupForm,
		verify: func(ctx context.Context, name string) error {
			ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			if cfg, err := LoadTunnelConfig(name); err == nil && cfg.Kernel.Enabled() {
				return ctVerifyKernel(ctx, cfg.Kernel)
			}
			for ctx.Err() == nil {
				if t, ok := Find(name); ok && TunnelHealth(t).Connected {
					return nil
				}
				ctSleep(ctx, time.Second)
			}
			return fmt.Errorf("the new tunnel did not connect to its peer: %w", ctx.Err())
		}}
}

func ctRemoveSetupForm(f PeerForm) error {
	cfg, err := LoadTunnelConfig(f.Name)
	if errors.Is(err, os.ErrNotExist) {
		return ctRemoveSetupAssets(f)
	}
	if err != nil {
		return err
	}
	token := cfg.Server.Token
	if f.Kind == "awg" {
		token = cfg.AWG.PresharedKey
		if token != ctSetupAWGKey(f.Token) {
			return fmt.Errorf("AmneziaWG identity changed; refusing cleanup")
		}
		token = f.Token
	} else if f.Kind == "kernel" {
		token = cfg.Kernel.Token
	} else if f.Kind == "direct" {
		token = cfg.L3.Token
	} else if f.Side == "kharej" {
		token = cfg.Client.Token
	}
	if token != f.Token {
		return fmt.Errorf("refusing to remove %s: its credential no longer belongs to this setup", f.Name)
	}
	if err := core.Delete(f.Name); err != nil {
		return err
	}
	return ctRemoveSetupAssets(f)
}

func ctValidateSetupForm(f PeerForm) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("automatic setup creates systemd services; run Backpack as root on both servers")
	}
	if !validName(f.Name) || fileExists(app.ConfigPath(f.Name)) {
		return fmt.Errorf("tunnel name %q is already used or invalid", f.Name)
	}
	if f.SetupConfig != nil {
		return ctValidatePromotedForm(f)
	}
	if f.Kind == "direct" {
		s, err := f.ToNewDirectTunnel().spec()
		if err != nil {
			return err
		}
		if why := kharejPortClash(s); why != "" {
			return fmt.Errorf("%s", why)
		}
		if s.Side == sideIran {
			if busy := busyForwardPorts(s.Ports, s.PeerIP); len(busy) > 0 {
				return fmt.Errorf("forwarded ports already in use: %s", strings.Join(busy, ", "))
			}
		}
		return nil
	}
	s, err := specFromNew(f.ToNewTunnel())
	if err != nil {
		return err
	}
	return refuseBusyForwards(s)
}

// ConnTestSetup owns a single deployment. Repeated submissions with the same
// ID read it back, including after a lost TCP/UDP reply; they never create a
// second pair. A fresh test is required for a second deployment.
type ConnTestSetup struct {
	mu       sync.Mutex
	v        ConnTestSetupView
	peerSeen time.Time
	request  *ConnTestSetupRequest
	kharej   PeerForm
	ack      ctSetupAck
	wake     chan struct{}
	results  []ConnTestResult
	link     ConnTestLink
	peer     string
	ops      ctSetupOps
	done     chan struct{}
}

func newCTSetup(link ConnTestLink, peer string, results []ConnTestResult) *ConnTestSetup {
	s := &ConnTestSetup{
		v:    ConnTestSetupView{State: "waiting", Expires: time.Now().Add(ConnTestSetupWindow).Unix()},
		wake: make(chan struct{}, 1), done: make(chan struct{}), results: append([]ConnTestResult(nil), results...),
		link: link, peer: peer, ops: ctDefaultSetupOps(),
	}
	for _, r := range results {
		if r.Status == ctOK && ctSetupSupported(r.Kind, r.Transport) {
			s.v.Choices = append(s.v.Choices, ConnTestSetupChoice{r.Kind, r.Transport, r.SNIDomain, r.Mbps})
		}
	}
	return s
}

func (s *ConnTestSetup) View() ConnTestSetupView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.v
	v.Ready = time.Since(s.peerSeen) < 30*time.Second && time.Now().Unix() < v.Expires
	return v
}

func (s *ConnTestSetup) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *ConnTestSetup) Start(r ConnTestSetupRequest) error {
	if r.ID == "" || len(r.ID) > 64 || len(r.Ports) > 1024 {
		return fmt.Errorf("a setup request ID and at most 1024 bytes of ports are required")
	}
	ports := parsePorts(r.Ports)
	if m, _, ok := ctKernelMethod(r.Transport); r.Kind == "kernel" && ok && m.IPv6Only && len(ports) > 0 {
		return fmt.Errorf("this kernel method creates a routed IPv6 tunnel; leave forwarded ports blank")
	}
	if len(ports) == 0 && r.Kind != "kernel" {
		return fmt.Errorf("at least one forwarded port is required")
	}
	if err := validatePortSpecs(ports); err != nil {
		return err
	}
	matched := false
	for _, row := range s.results {
		if row.Kind == r.Kind && row.Transport == r.Transport && row.SNIDomain == r.SNIDomain && row.Status == ctOK && ctSetupSupported(row.Kind, row.Transport) {
			matched = true
		}
	}
	if !matched {
		return fmt.Errorf("choose a tunnel that passed this connection test")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.request != nil {
		if *s.request == r {
			return nil
		}
		return fmt.Errorf("this test already has a setup request; start a new test for another tunnel")
	}
	if s.v.State != "waiting" || time.Now().Unix() >= s.v.Expires {
		return fmt.Errorf("the automatic setup session has ended; run a new test")
	}
	if time.Since(s.peerSeen) >= 30*time.Second {
		return fmt.Errorf("the kharej setup agent is not ready; keep the updated Backpack test open there")
	}
	s.request = &r
	s.v.ID, s.v.State = r.ID, "preparing"
	s.signal()
	return nil
}

func ctSetupEncode(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func ctSetupDecode(raw string, v any) error {
	if len(raw) > 6000 {
		return fmt.Errorf("setup message too large")
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Called only after the coordinator's constant-time token check.
func (s *ConnTestSetup) answer(action, payload string) string {
	if action == "setup-start" {
		var r ConnTestSetupRequest
		if err := ctSetupDecode(payload, &r); err != nil {
			return "error invalid setup request"
		}
		if err := s.Start(r); err != nil {
			return "error " + err.Error()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if action == "setup-poll" {
		s.peerSeen = time.Now() // only after the kharej's test engines are gone
	}
	if action == "setup-ack" {
		var ack ctSetupAck
		if ctSetupDecode(payload, &ack) != nil || ack.ID != s.v.ID || ack.ID == "" {
			return "error invalid setup acknowledgement"
		}
		// A retried prepare acknowledgement cannot undo an applied one.
		if (s.v.State == "preparing" && ack.Stage == "prepared") ||
			(s.v.State == "applying" && ack.Stage == "applied") ||
			(s.v.State == "done" && ack.Stage == "confirmed") {
			s.ack = ack
			s.signal()
		}
	}
	w := ctSetupWire{View: s.v}
	w.View.Choices = nil // the peer already has its rows; keep control replies small
	w.View.Ready = time.Since(s.peerSeen) < 30*time.Second
	if s.kharej.Name != "" && (s.v.State == "preparing" || s.v.State == "applying") {
		f := s.kharej
		w.Form = &f
	}
	return "setup " + ctSetupEncode(w)
}

func (s *ConnTestSetup) setState(state, why string) {
	s.mu.Lock()
	s.v.State, s.v.Error = state, why
	s.mu.Unlock()
}

func (s *ConnTestSetup) waitAck(ctx context.Context, stage string) error {
	for {
		s.mu.Lock()
		a := s.ack
		s.mu.Unlock()
		if a.Stage == stage {
			if a.Error != "" {
				return fmt.Errorf("kharej: %s", a.Error)
			}
			return nil
		}
		select {
		case <-s.wake:
		case <-ctx.Done():
			return fmt.Errorf("waiting for kharej %s: %w", stage, ctx.Err())
		}
	}
}

// forms preserves the exact tested port, transport and paired tuning. It
// replaces throwaway echo mappings and credentials; neither is reused in the
// permanent tunnel. The direct pair keeps its tested private subnet.
func (s *ConnTestSetup) forms(r ConnTestSetupRequest) (PeerForm, PeerForm, error) {
	var l ShareLink
	if r.Transport == "ssh" {
		return PeerForm{}, PeerForm{}, fmt.Errorf("SSH needs a production sshd login and pinned host key; use manual setup")
	}

	for _, c := range s.link.Cases {
		if c.Kind == r.Kind && c.Tr == r.Transport && c.SNI == r.SNIDomain {
			l = c
			break
		}
	}
	if l.Kind == "" {
		return PeerForm{}, PeerForm{}, fmt.Errorf("the tested tunnel settings are missing")
	}
	caseID := ctSetupCaseID(l)
	l.Name = "quick-" + randomToken(8)
	l.Tok, l.Ports, l.AcceptUDP = randomToken(64), r.Ports, r.AcceptUDP
	if l.Kind == "awg" || l.Kind == "kernel" {
		iran := PeerForm{Kind: l.Kind, Transport: l.Tr, Side: "iran", Name: l.Name + "-iran", Token: l.Tok, TunnelPort: l.Port, Ports: r.Ports, AcceptUDP: r.AcceptUDP, TestCase: caseID}
		kharej := iran
		kharej.Side, kharej.Name, kharej.Ports = "kharej", l.Name+"-kharej", ""
		return iran, kharej, nil
	}
	l.From, l.Host = "iran", s.link.Host
	kharej := MirrorForPeer(l)
	// Invert once more to build Iran, with the correct dialling address for
	// a direct tunnel. Ports belong to Iran in both engines.
	l.From, l.Host = "kharej", s.peer
	if l.Kind == "direct" {
		l.LocalIP, l.PeerIP = kharej.LocalIP, hostOf(l.LocalIP)
	}
	iran := MirrorForPeer(l)
	iran.TestCase, kharej.TestCase = caseID, caseID
	return iran, kharej, nil
}

// Run waits for one request and carries out both ends in order. A failed
// deployment removes only the newly assigned pair, never an existing tunnel.
func (s *ConnTestSetup) Run(parent context.Context) {
	defer close(s.done)
	ctx, cancel := context.WithDeadline(parent, time.Unix(s.v.Expires, 0))
	defer cancel()
	for {
		s.mu.Lock()
		r := s.request
		s.mu.Unlock()
		if r != nil {
			s.deploy(ctx, *r)
			return
		}
		select {
		case <-s.wake:
		case <-ctx.Done():
			s.setState("expired", "the automatic setup session ended")
			return
		}
	}
}

func (s *ConnTestSetup) deploy(parent context.Context, r ConnTestSetupRequest) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	iran, kharej, err := s.forms(r)
	localAttempted := false
	defer func() {
		if err != nil {
			if localAttempted {
				if cleanup := s.ops.remove(iran); cleanup != nil {
					err = fmt.Errorf("%w; Iran cleanup failed for %s: %v", err, iran.Name, cleanup)
				}
			}
			s.setState("failed", err.Error())
		}
	}()
	if err != nil {
		return
	}
	if s.ops.prepare != nil {
		localAttempted = true
		iran, err = s.ops.prepare(iran)
		if err != nil {
			return
		}
	}
	if err = s.ops.validate(iran); err != nil {
		return
	}
	s.mu.Lock()
	s.kharej = kharej
	s.v.Iran, s.v.Kharej = iran.Name, kharej.Name
	s.mu.Unlock()
	if err = s.waitAck(ctx, "prepared"); err != nil {
		return
	}
	localAttempted = true
	var active bool
	_, active, err = s.ops.apply(iran)
	if err == nil && !active {
		err = fmt.Errorf("Iran tunnel service did not start")
	}
	if err != nil {
		return
	}
	s.setState("applying", "")
	if err = s.waitAck(ctx, "applied"); err != nil {
		return
	}
	s.setState("verifying", "")
	if err = s.ops.verify(ctx, iran.Name); err != nil {
		return
	}
	s.setState("done", "")
	// A terminal can close this session as soon as it sees success. Keep the
	// coordinator alive until the kharej has collected that final decision;
	// otherwise it could interpret the closed coordinator as an aborted setup.
	receiptCtx, receiptCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer receiptCancel()
	_ = s.waitAck(receiptCtx, "confirmed")
}

// EnableSetup is called after the verdict has been collected. Test engines
// stop before either permanent end can bind its tested port again.
func (s *ConnTestIran) EnableSetup(ctx context.Context, results []ConnTestResult) *ConnTestSetup {
	s.mu.Lock()
	engines := s.engines
	s.engines = nil
	s.mu.Unlock()
	ctStopAll(engines)
	setup := newCTSetup(s.link, s.Kharej(), results)
	if material, err := ctCaptureSetupMaterial(s.link, s.dir); err == nil {
		setup.ops.prepare = material.prepare
	} else {
		setup.ops.prepare = func(f PeerForm) (PeerForm, error) { return f, err }
	}
	// Direct listener ports are negotiated by the peer during the test;
	// select the measured endpoint, including the exact SNI candidate.
	setup.link.Cases = nil
	for _, c := range s.cases {
		if !ctSetupSupported(c.kind, c.tr) {
			continue
		}
		l := c.link
		if c.kind == "direct" {
			l.Port = addrPort(c.l3.Addr)
		}
		setup.link.Cases = append(setup.link.Cases, l)
	}
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.setupCancel = cancel
	s.mu.Unlock()
	s.coord.mu.Lock()
	s.coord.setup = setup
	s.coord.mu.Unlock()
	go setup.Run(ctx)
	return setup
}

// RunConnTestSetupPeer keeps the kharej available after its test engines have
// stopped. Requests and acknowledgements are retried; apply runs once per ID.
func RunConnTestSetupPeer(ctx context.Context, raw string, update func(ConnTestSetupView)) error {
	a, err := parseConnTestLink(raw)
	if err != nil {
		return err
	}
	ops := ctDefaultSetupOps()
	material := ctGetSetupMaterial(a)
	if material == nil {
		return fmt.Errorf("this test's setup identities are unavailable; update both servers and run a new test")
	}
	ops.prepare = material.prepare
	return ctRunSetupPeer(ctx, a, ops, update)
}

func ctRunSetupPeer(parent context.Context, a connTestAddr, ops ctSetupOps, update func(ConnTestSetupView)) (resultErr error) {
	ctx, cancel := context.WithTimeout(parent, ConnTestSetupWindow+time.Minute)
	defer cancel()
	var prepared, applied string
	var form PeerForm
	var ack *ctSetupAck
	attempted, committed := false, false
	lastAnswer := time.Now()
	defer func() {
		if attempted && !committed {
			if err := ops.remove(form); err != nil {
				resultErr = fmt.Errorf("%v; kharej cleanup failed for %s: %w", resultErr, form.Name, err)
			}
		}
	}()
	for ctx.Err() == nil {
		line := "setup-poll " + a.Tok
		if ack != nil {
			line = "setup-ack " + a.Tok + " " + ctSetupEncode(ack)
		}
		reply, askErr := ctAskContext(ctx, a.Host, a.Coord, line)
		if askErr == nil && strings.HasPrefix(reply, "setup ") {
			lastAnswer = time.Now()
			var w ctSetupWire
			if ctSetupDecode(strings.TrimPrefix(reply, "setup "), &w) != nil {
				return fmt.Errorf("damaged automatic setup response")
			}
			if update != nil {
				update(w.View)
			}
			switch w.View.State {
			case "done":
				if applied == "" || applied != w.View.ID {
					return fmt.Errorf("setup completed without this kharej's acknowledgement")
				}
				committed = true
				if ack != nil && ack.Stage == "confirmed" {
					return nil
				}
				ack = &ctSetupAck{ID: w.View.ID, Stage: "confirmed"}
			case "failed", "expired":
				return fmt.Errorf("%s", w.View.Error)
			case "preparing":
				if w.Form != nil && prepared == "" {
					form = *w.Form
					ack = &ctSetupAck{ID: w.View.ID, Stage: "prepared"}
					var prepErr error
					if ops.prepare != nil {
						attempted = true
						form, prepErr = ops.prepare(form)
					}
					if prepErr != nil {
						ack.Error = prepErr.Error()
					} else if err := ops.validate(form); err != nil {
						ack.Error = err.Error()
					}
					prepared = w.View.ID
				}
			case "applying":
				if w.Form != nil && prepared == w.View.ID && ack != nil && ack.Error == "" && applied == "" {
					attempted = true
					_, active, err := ops.apply(form)
					ack = &ctSetupAck{ID: w.View.ID, Stage: "applied"}
					if err != nil {
						ack.Error = err.Error()
					} else if !active {
						ack.Error = "tunnel service did not start"
					}
					applied = w.View.ID
				}
			}
		}
		if time.Since(lastAnswer) > 30*time.Second {
			return fmt.Errorf("the Iran automatic setup coordinator stopped answering; keep the test open and update both servers")
		}
		ctSleep(ctx, time.Second)
	}
	return fmt.Errorf("automatic setup stopped before completion: %w", ctx.Err())
}

// StartConnTestSetupFromKharej submits the same request as the Iran panel.
func StartConnTestSetupFromKharej(raw string, r ConnTestSetupRequest) error {
	a, err := parseConnTestLink(raw)
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("automatic setup creates services on both servers; run Backpack as root")
	}
	reply, err := ctAskContext(context.Background(), a.Host, a.Coord, "setup-start "+a.Tok+" "+ctSetupEncode(r))
	if err != nil {
		return err
	}
	if strings.HasPrefix(reply, "error ") {
		return fmt.Errorf("%s", strings.TrimPrefix(reply, "error "))
	}
	if !strings.HasPrefix(reply, "setup ") {
		return fmt.Errorf("the Iran server does not support automatic setup; update both servers")
	}
	return nil
}
