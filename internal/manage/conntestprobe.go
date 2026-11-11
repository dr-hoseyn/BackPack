package manage

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"time"
)

// ctProbe pushes traffic through one tunnel and judges it.
// report is told the tunnel's state whenever it changes: after every echo,
// and once more with the verdict, which is also returned.
func ctProbe(ctx context.Context, c *connTestCase, l3Echo int, report func(ConnTestResult)) ConnTestResult {
	r := c.initialResult()
	say := func(string) {
		if report != nil {
			report(r)
		}
	}

	// Up: the first echo that comes back.
	start := time.Now()
	connectCtx, cancelConnect := context.WithTimeout(ctx, connectWaitFor(c))
	defer cancelConnect()
	var conn net.Conn
	for conn == nil {
		if ctx.Err() != nil {
			r.Status, r.Detail = ctDown, "stopped"
			say("")
			return r
		}
		if connectCtx.Err() != nil {
			r.Status, r.Detail = ctDown, fmt.Sprintf("never carried traffic in %s", connectWaitFor(c))
			if c.tr == "spoof" {
				r.Detail += " — the forged source was dropped: by the provider on the way out, " +
					"or by a strict rp_filter on the kharej"
			}
			say("never came up")
			return r
		}
		if cn, err := ctDial(connectCtx, c, l3Echo); err == nil {
			echoErr := ctEchoContext(connectCtx, cn, c.udp)
			if echoErr == nil {
				conn = cn
				break
			}
			cn.Close()
		}
		retry := time.Second
		if c.kind == "kernel" && c.mode == ConnTestQuick {
			retry = 100 * time.Millisecond
		}
		ctSleep(connectCtx, retry)
	}
	r.Connect = time.Since(start).Seconds()
	say(fmt.Sprintf("up after %.0fs", r.Connect))

	r.FreshOK, r.FreshTried = 1, 1
	freshCtx, stopFresh := context.WithTimeout(ctx, min(4*time.Second, time.Duration(r.Total)*time.Second))
	defer stopFresh()
	fresh := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		go func() {
			cn, err := ctDial(freshCtx, c, l3Echo)
			if err != nil {
				fresh <- false
				return
			}
			defer cn.Close()
			fresh <- ctEchoContext(freshCtx, cn, c.udp) == nil
		}()
	}
	for i := 0; i < 2; i++ {
		r.FreshTried++
		if <-fresh {
			r.FreshOK++
		}
	}

	stopFresh()
	// Staying up: an echo a second over the same connection, and a new one
	// only when that one is lost — which is exactly what a user's connection
	// would see.
	var rtt time.Duration
	var firstLoss time.Duration
	soakStart := time.Now()
	soakEnd := soakStart.Add(time.Duration(r.Total) * time.Second)
	soakCtx, cancelSoak := context.WithDeadline(ctx, soakEnd)
	defer cancelSoak()
	for i := 0; i < r.Total && ctx.Err() == nil; i++ {
		due := soakStart.Add(time.Duration(i) * time.Second)
		ctSleep(soakCtx, time.Until(due))
		if ctx.Err() != nil {
			break
		}
		r.Tried++
		if soakCtx.Err() != nil || time.Now().After(due.Add(time.Second)) {
			if firstLoss == 0 {
				firstLoss = time.Since(soakStart)
			}
			say("")
			continue
		}
		if conn == nil {
			cn, err := ctDial(soakCtx, c, l3Echo)
			if err != nil {
				if firstLoss == 0 {
					firstLoss = time.Since(soakStart)
				}
				say("")
				continue
			}
			conn = cn
		}
		t0 := time.Now()
		err := ctEchoContext(soakCtx, conn, c.udp)
		if err != nil {
			conn.Close()
			conn = nil
			if firstLoss == 0 {
				firstLoss = time.Since(soakStart)
			}
			say("")
			continue
		}
		r.OK++
		took := time.Since(t0)
		rtt += took
		c.rtts = append(c.rtts, took)
		say("")
	}
	if r.Tried == r.Total {
		ctSleep(ctx, time.Until(soakEnd))
	}
	if conn != nil {
		conn.Close()
	}
	r.Observed = time.Since(soakStart).Seconds()
	if len(c.rtts) > 0 {
		sorted := append([]time.Duration(nil), c.rtts...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		r.P95ms = float64(sorted[(len(sorted)*95-1)/100]) / float64(time.Millisecond)
	}
	if r.OK > 0 {
		r.RTTms = int((rtt / time.Duration(r.OK)).Milliseconds())
		r.JitterMs = float64(ctQuality(r, c.rtts).Jitter) / float64(time.Millisecond)
		r.JitterMeasured = len(c.rtts) >= 2
	}

	switch {
	case ctx.Err() != nil:
		r.Status, r.Detail = ctUnstable, "stopped before the test completed"
	case r.OK == r.Total && r.Tried == r.Total && r.FreshOK == 3:
		if c.udp {
			r.Status = ctOK
		} else {
			r.Phase, r.Detail = "speed-queued", "stability passed; waiting for speed test"
		}
	case r.FreshOK != 3:
		r.Status, r.Detail = ctUnstable, fmt.Sprintf("%d/%d independent payload connections passed", r.FreshOK, r.FreshTried)
	case r.OK == 0:
		r.Status = ctDown
		r.Detail = fmt.Sprintf("came up, then carried nothing (lost after %.0fs)", firstLoss.Seconds())
	default:
		r.Status = ctUnstable
		if firstLoss > 0 {
			r.Detail = fmt.Sprintf("first loss after %.0fs", firstLoss.Seconds())
		}
	}
	say("done: " + r.Status)
	return r
}

func ctDial(ctx context.Context, c *connTestCase, l3Echo int) (net.Conn, error) {
	network, addr := "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(c.entry))
	if c.udp {
		network = "udp"
	}
	if c.kind == "direct" || c.kind == "kernel" {
		addr = net.JoinHostPort(c.peerIP, strconv.Itoa(l3Echo))
	}
	return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, addr)
}

// connectWaitFor is how long a tunnel is given to come up. The direct ones
// wait for the kharej to have built its interface as well.
func connectWaitFor(c *connTestCase) time.Duration {
	if c.kind == "kernel" {
		return ctKernelConnectWait(c)
	}
	if c.mode == ConnTestQuick {
		if c.kind == "direct" {
			return 30 * time.Second
		}
		return 20 * time.Second
	}
	if c.kind == "direct" {
		return connTestConnectWait + 30*time.Second
	}
	return connTestConnectWait
}

// ctEcho sends a small random payload and requires it back unchanged.
func ctEcho(c net.Conn, udp bool) error {
	msg := make([]byte, 512)
	_, _ = rand.Read(msg)
	_ = c.SetDeadline(time.Now().Add(4 * time.Second))
	defer c.SetDeadline(time.Time{})
	if n, err := c.Write(msg); err != nil {
		return err
	} else if n != len(msg) {
		return io.ErrShortWrite
	}
	got := make([]byte, len(msg))
	if udp {
		n, err := c.Read(got)
		if err != nil {
			return err
		}
		got = got[:n]
	} else if _, err := io.ReadFull(c, got); err != nil {
		return err
	}
	if !bytes.Equal(got, msg) {
		return errors.New("the echo came back different")
	}
	return nil
}

// ctBulkContext exercises the legacy echo transfer for compatibility checks.
// Production speed measurements use ctSpeed after all stability probes finish.
func ctBulkContext(ctx context.Context, dial func() (net.Conn, error)) (float64, error) {
	c, err := dial()
	if err != nil {
		return 0, err
	}
	defer c.Close()
	stopClose := context.AfterFunc(ctx, func() { c.Close() })
	defer stopClose()
	_ = c.SetDeadline(time.Now().Add(40 * time.Second))
	payload := make([]byte, connTestBulk)
	_, _ = rand.Read(payload)
	start := time.Now()
	werr := make(chan error, 1)
	go func() {
		n, err := c.Write(payload)
		if err == nil && n != len(payload) {
			err = io.ErrShortWrite
		}
		werr <- err
	}()
	got := make([]byte, len(payload))
	if n, err := io.ReadFull(c, got); err != nil {
		c.Close()
		<-werr
		return 0, fmt.Errorf("%d of %d KB back: %w", n>>10, len(payload)>>10, err)
	}
	if err := <-werr; err != nil {
		return 0, err
	}
	if !bytes.Equal(got, payload) {
		return 0, errors.New("the transfer came back different")
	}
	secs := time.Since(start).Seconds()
	return float64(2*connTestBulk*8) / secs / 1e6, nil
}

func ctSleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
