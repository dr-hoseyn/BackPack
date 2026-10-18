package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"math"
	"net"
	"slices"
	"time"
)

type soakOutage struct {
	Start   time.Time  `json:"start_utc"`
	End     *time.Time `json:"end_utc,omitempty"`
	Seconds float64    `json:"seconds"`
}
type soakResult struct {
	Complete           bool         `json:"complete"`
	Attempted          uint64       `json:"attempted"`
	Received           uint64       `json:"received"`
	LossPercent        float64      `json:"unanswered_probe_percent"`
	P95RTTMillis       float64      `json:"rtt_p95_ms"`
	JitterMillis       float64      `json:"successive_rtt_jitter_ms"`
	MaxRecoverySeconds float64      `json:"max_observed_recovery_seconds"`
	Outages            []soakOutage `json:"outages"`
}

func soakProbe(ctx context.Context, endpoint string, chunk int) (time.Duration, error) {
	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	body := make([]byte, chunk)
	if _, err := rand.Read(body); err != nil {
		return 0, err
	}
	if _, err := io.Copy(conn, bytes.NewReader(body)); err != nil {
		return 0, err
	}
	read := make([]byte, chunk)
	if _, err := io.ReadFull(conn, read); err != nil {
		return 0, err
	}
	if !bytes.Equal(body, read) {
		return 0, errors.New("soak payload failed verification")
	}
	return time.Since(start), nil
}

// Each fresh connection verifies random bytes; recovery is an observation from
// the first failed probe to the next successful payload, including sampling
// cadence and timeout. It is not packet loss or a capacity benchmark.
func runSoak(parent context.Context, endpoint string, duration, timeout time.Duration, chunk int, pids []int32) (result, error) {
	r := result{Mode: "soak", Endpoint: endpoint, Parallel: 1, ChunkBytes: chunk, IOTimeoutSeconds: timeout.Seconds(), StartedAt: time.Now().UTC(), Soak: &soakResult{}}
	if duration <= 0 || duration > time.Hour || timeout <= 0 || chunk < 1 || chunk > 1<<20 {
		return r, errors.New("invalid soak bounds")
	}
	start := time.Now()
	deadline := start.Add(duration)
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	var sm *sampler
	if len(pids) > 0 {
		sm = &sampler{roots: pids, last: map[string]float64{}, start: start.UnixMilli()}
		sm.sample()
	}
	var samples []float64
	var jitter, previous float64
	var pairs int
	var outage *time.Time
	for ctx.Err() == nil {
		at := time.Now()
		probeCtx, stop := context.WithTimeout(ctx, timeout)
		latency, err := soakProbe(probeCtx, endpoint, chunk)
		stop()
		// A boundary cancellation is not an attempted completed measurement.
		if ctx.Err() != nil {
			break
		}
		r.Soak.Attempted++
		if err == nil {
			r.Soak.Received++
			r.VerifiedBytes += uint64(chunk)
			ms := float64(latency) / float64(time.Millisecond)
			samples = append(samples, ms)
			if previous > 0 {
				jitter += math.Abs(ms - previous)
				pairs++
			}
			previous = ms
			if outage != nil {
				end := time.Now().UTC()
				seconds := end.Sub(*outage).Seconds()
				r.Soak.Outages = append(r.Soak.Outages, soakOutage{*outage, &end, seconds})
				r.Soak.MaxRecoverySeconds = max(r.Soak.MaxRecoverySeconds, seconds)
				outage = nil
			}
		} else {
			r.Failures++
			previous = 0
			if outage == nil {
				v := at.UTC()
				outage = &v
			}
		}
		if sm != nil {
			sm.sample()
		}
		delay := time.Second - time.Since(at)
		if delay > 0 {
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
			case <-t.C:
			}
		}
	}
	r.Seconds = time.Since(start).Seconds()
	r.Soak.Complete = parent.Err() == nil && !time.Now().Before(deadline)
	if outage != nil {
		r.Soak.Outages = append(r.Soak.Outages, soakOutage{Start: *outage, Seconds: time.Since(*outage).Seconds()})
	}
	if r.Soak.Attempted > 0 {
		r.Soak.LossPercent = 100 * float64(r.Failures) / float64(r.Soak.Attempted)
	}
	if len(samples) > 0 {
		slices.Sort(samples)
		r.Soak.P95RTTMillis = samples[int(math.Ceil(float64(len(samples))*.95))-1]
	}
	if pairs > 0 {
		r.Soak.JitterMillis = jitter / float64(pairs)
	}
	if sm != nil {
		sm.sample()
		r.Resources = &sm.result
		if r.Seconds > 0 {
			r.Resources.CPUPercent = 100 * r.Resources.CPUSeconds / r.Seconds
		}
	}
	if parent.Err() != nil {
		return r, parent.Err()
	}
	return r, nil
}
