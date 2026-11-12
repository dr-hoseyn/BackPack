package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var errCorruption = errors.New("payload corruption")

type probeOutcome struct {
	Number              int     `json:"number"`
	ElapsedMS           float64 `json:"elapsed_ms"`
	DialMS              float64 `json:"dial_ms"`
	DispatchDelayMS     float64 `json:"dispatch_delay_ms"`
	ScheduledToResultMS float64 `json:"scheduled_to_result_ms"`
	Error               string  `json:"error,omitempty"`
}

func percentile(values []float64, fraction float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	return values[max(0, min(int(math.Ceil(float64(len(values))*fraction))-1, len(values)-1))]
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func exchange(c net.Conn, p, reply []byte) error {
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(p)))
	if _, err := c.Write(h[:]); err != nil {
		return err
	}
	if _, err := io.Copy(c, bytes.NewReader(p)); err != nil {
		return err
	}
	if _, err := io.ReadFull(c, reply[:len(p)]); err != nil {
		return err
	}
	if !bytes.Equal(p, reply[:len(p)]) {
		return errCorruption
	}
	return nil
}
func echo(addr string) {
	l, err := net.Listen("tcp", addr)
	must(err)
	defer l.Close()
	fmt.Println("READY")
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			var h [4]byte
			var b []byte
			for {
				if _, err := io.ReadFull(c, h[:]); err != nil {
					return
				}
				n := int(binary.BigEndian.Uint32(h[:]))
				if n < 0 || n > 1<<20 {
					return
				}
				if cap(b) < n {
					b = make([]byte, n)
				} else {
					b = b[:n]
				}
				if _, err := io.ReadFull(c, b[:n]); err != nil {
					return
				}
				if _, err := io.Copy(c, bytes.NewReader(b[:n])); err != nil {
					return
				}
			}
		}()
	}
}
func proxy(addr, target, marker string) {
	l, err := net.Listen("tcp", addr)
	must(err)
	defer l.Close()
	fmt.Println("READY")
	var index int
	for {
		client, err := l.Accept()
		if err != nil {
			return
		}
		index++
		selected := index == 2
		go func() {
			defer client.Close()
			remote, err := net.DialTimeout("tcp", target, 5*time.Second)
			if err != nil {
				return
			}
			defer remote.Close()
			if selected {
				_ = remote.(*net.TCPConn).SetReadBuffer(16384)
			}
			done := make(chan struct{})
			go func() { defer close(done); _, _ = io.Copy(remote, client); remote.Close() }()
			b := make([]byte, 32768)
			paused := false
			for {
				n, err := remote.Read(b)
				if n > 0 {
					if selected && !paused && marker != "" {
						if _, e := os.Stat(marker); e == nil {
							paused = true
							fmt.Println("STALL_BEGIN", time.Now().UnixNano())
							time.Sleep(6 * time.Second)
							fmt.Println("STALL_END", time.Now().UnixNano())
						}
					}
					if _, e := io.Copy(client, bytes.NewReader(b[:n])); e != nil {
						break
					}
				}
				if err != nil {
					break
				}
			}
			client.Close()
			remote.Close()
			<-done
		}()
	}
}
func load(addr string, seconds, workers, idle int, mbps float64) {
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for range idle {
		c, e := net.DialTimeout("tcp", addr, 5*time.Second)
		must(e)
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		must(exchange(c, []byte("idle-ready"), make([]byte, 10)))
		_ = c.SetDeadline(time.Time{})
		held = append(held, c)
	}
	fmt.Println("LOAD_READY")
	start := time.Now()
	deadline := start.Add(time.Duration(seconds) * time.Second)
	var transferred, bulkErrors, probeErrors, corruptionErrors atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, e := net.DialTimeout("tcp", addr, 5*time.Second)
			if e != nil {
				bulkErrors.Add(1)
				return
			}
			defer c.Close()
			p := make([]byte, 256*1024)
			_, _ = rand.Read(p)
			r := make([]byte, len(p))
			next := time.Now()
			interval := time.Duration(float64(len(p)) * 8 / (mbps * 1e6 / float64(workers)) * float64(time.Second))
			for time.Now().Before(deadline) {
				_ = c.SetDeadline(time.Now().Add(15 * time.Second))
				if e := exchange(c, p, r); e != nil {
					if errors.Is(e, errCorruption) {
						corruptionErrors.Add(1)
					}
					bulkErrors.Add(1)
					return
				}
				transferred.Add(int64(len(p)))
				next = next.Add(interval)
				if d := time.Until(next); d > 0 {
					time.Sleep(d)
				}
			}
		}()
	}
	var mu sync.Mutex
	var latencies []float64
	var outcomes []probeOutcome
	scheduled, skipped := 0, 0
	slots := make(chan struct{}, 4)
	ticker := time.NewTicker(100 * time.Millisecond)
	for now := range ticker.C {
		if now.After(deadline) {
			ticker.Stop()
			break
		}
		scheduled++
		select {
		case slots <- struct{}{}:
		default:
			skipped++
			continue
		}
		probeNumber := scheduled
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			before := time.Now()
			var dialMS float64
			var probeError error
			defer func() {
				outcome := probeOutcome{
					Number: probeNumber, ElapsedMS: float64(time.Since(before)) / float64(time.Millisecond),
					DialMS: dialMS, DispatchDelayMS: float64(before.Sub(now)) / float64(time.Millisecond),
					ScheduledToResultMS: float64(time.Since(now)) / float64(time.Millisecond),
				}
				if probeError != nil {
					probeErrors.Add(1)
					outcome.Error = "network"
					var networkError net.Error
					if errors.As(probeError, &networkError) && networkError.Timeout() {
						outcome.Error = "timeout"
					}
					if errors.Is(probeError, errCorruption) {
						corruptionErrors.Add(1)
						outcome.Error = "corruption"
					}
				}
				mu.Lock()
				outcomes = append(outcomes, outcome)
				if probeError == nil {
					latencies = append(latencies, outcome.ElapsedMS)
				}
				mu.Unlock()
			}()
			c, e := net.DialTimeout("tcp", addr, 4*time.Second)
			dialMS = float64(time.Since(before)) / float64(time.Millisecond)
			if e != nil {
				probeError = e
				return
			}
			defer c.Close()
			_ = c.SetDeadline(before.Add(4 * time.Second))
			p := make([]byte, 64)
			_, _ = rand.Read(p)
			if e := exchange(c, p, make([]byte, 64)); e != nil {
				probeError = e
				return
			}
		}()
	}
	wg.Wait()
	scheduledLatencies := make([]float64, 0, len(outcomes))
	allLatencies := make([]float64, 0, len(outcomes))
	for _, outcome := range outcomes {
		allLatencies = append(allLatencies, outcome.ElapsedMS)
		scheduledLatencies = append(scheduledLatencies, outcome.ScheduledToResultMS)
	}
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].Number < outcomes[j].Number })
	pct := func(p float64) float64 {
		return percentile(latencies, p)
	}
	// Silent users must still be usable after bulk load and carrier pressure.
	idleVerifiedAfter, idleErrors := 0, 0
	for _, c := range held {
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		if e := exchange(c, []byte("still-alive"), make([]byte, 11)); e != nil {
			if errors.Is(e, errCorruption) {
				corruptionErrors.Add(1)
			}
			idleErrors++
		} else {
			idleVerifiedAfter++
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"seconds": time.Since(start).Seconds(), "bytes": transferred.Load(), "application_mbps": float64(transferred.Load()) * 8 / time.Since(start).Seconds() / 1e6, "bulk_errors": bulkErrors.Load(), "corruption_errors": corruptionErrors.Load(), "probe_errors": probeErrors.Load(), "probes": len(latencies), "p50_ms": pct(.5), "p95_ms": pct(.95), "p99_ms": pct(.99), "probe_scheduled": scheduled, "probe_attempted": len(outcomes), "probe_skipped_at_concurrency_cap": skipped, "p95_all_attempts_ms": percentile(allLatencies, .95), "p99_all_attempts_ms": percentile(allLatencies, .99), "probe_outcomes": outcomes, "idle_verified": len(held), "idle_verified_after_load": idleVerifiedAfter, "idle_errors": idleErrors, "probe_measurement_version": 2, "p95_scheduled_to_result_ms": percentile(scheduledLatencies, .95), "p99_scheduled_to_result_ms": percentile(scheduledLatencies, .99)})
}
func main() {
	mode := flag.String("mode", "load", "")
	addr := flag.String("addr", "127.0.0.1:47002", "")
	target := flag.String("target", "", " ")
	marker := flag.String("marker", "", "")
	seconds := flag.Int("seconds", 15, "")
	workers := flag.Int("workers", 8, "")
	idle := flag.Int("idle", 32, "")
	mbps := flag.Float64("mbps", 20, "")
	flag.Parse()
	switch *mode {
	case "echo":
		echo(*addr)
	case "proxy":
		proxy(*addr, *target, *marker)
	case "load":
		load(*addr, *seconds, *workers, *idle, *mbps)
	default:
		panic("invalid mode")
	}
}
