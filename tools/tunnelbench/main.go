// tunnelbench measures verified TCP echo transfers through already configured tunnels.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/shirou/gopsutil/v4/process"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type stringsFlag []string

func (v *stringsFlag) String() string     { return strings.Join(*v, ",") }
func (v *stringsFlag) Set(s string) error { *v = append(*v, s); return nil }

type result struct {
	Mode             string          `json:"mode,omitempty"`
	Direction        string          `json:"direction,omitempty"`
	WarmupSeconds    float64         `json:"warmup_seconds,omitempty"`
	BulkMbps         float64         `json:"verified_bulk_mbps,omitempty"`
	Name             string          `json:"name"`
	StartedAt        time.Time       `json:"started_at_utc"`
	Endpoint         string          `json:"endpoint"`
	Parallel         int             `json:"parallel"`
	Seconds          float64         `json:"seconds"`
	ChunkBytes       int             `json:"chunk_bytes"`
	IOTimeoutSeconds float64         `json:"io_timeout_seconds"`
	VerifiedBytes    uint64          `json:"verified_bytes"`
	Mbps             float64         `json:"verified_echo_mbps"`
	Failures         uint64          `json:"failures"`
	Resources        *resourceResult `json:"local_resources,omitempty"`
	Soak             *soakResult     `json:"soak,omitempty"`
}
type resourceResult struct {
	CPUSeconds float64 `json:"cpu_seconds"`
	CPUPercent float64 `json:"cpu_percent_one_core_100"`
	PeakRSS    uint64  `json:"peak_aggregate_rss_bytes"`
	Samples    int     `json:"samples"`
	Errors     int     `json:"sampling_errors"`
}
type sampler struct {
	roots  []int32
	last   map[string]float64
	start  int64
	result resourceResult
}

func (s *sampler) sample() {
	seen := map[int32]bool{}
	queue := append([]int32(nil), s.roots...)
	var rss uint64
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		p, err := process.NewProcess(pid)
		if err != nil {
			s.result.Errors++
			continue
		}
		created, err := p.CreateTime()
		if err != nil {
			s.result.Errors++
			continue
		}
		times, err := p.Times()
		if err != nil {
			s.result.Errors++
			continue
		}
		key := fmt.Sprintf("%d/%d", pid, created)
		cpu := times.User + times.System
		if previous, ok := s.last[key]; ok {
			s.result.CPUSeconds += max(0, cpu-previous)
		} else if created >= s.start {
			s.result.CPUSeconds += cpu
		}
		s.last[key] = cpu
		memory, err := p.MemoryInfo()
		if err == nil {
			rss += memory.RSS
		} else {
			s.result.Errors++
		}
		children, err := p.Children()
		if err != nil {
			s.result.Errors++
		} else {
			for _, child := range children {
				queue = append(queue, child.Pid)
			}
		}
	}
	s.result.Samples++
	s.result.PeakRSS = max(s.result.PeakRSS, rss)
}
func runCase(parent context.Context, endpoint string, parallel int, duration time.Duration, chunk int, ioTimeout time.Duration, pids []int32) (result, error) {
	r := result{Endpoint: endpoint, Parallel: parallel, ChunkBytes: chunk, IOTimeoutSeconds: ioTimeout.Seconds()}
	if parallel < 1 || parallel > 256 || chunk < 1 || chunk > 1<<20 || duration <= 0 || ioTimeout <= 0 {
		return r, errors.New("invalid benchmark bounds")
	}
	// Establish all connections before the sampling window. A setup failure
	// fails the case instead of comparing a silently smaller concurrency.
	conns := make([]net.Conn, 0, parallel)
	defer func() {
		for _, conn := range conns {
			conn.Close()
		}
	}()
	for range parallel {
		ctx, cancel := context.WithTimeout(parent, 3*time.Second)
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", endpoint)
		cancel()
		if err != nil {
			return r, fmt.Errorf("establish %d connections: %w", parallel, err)
		}
		conns = append(conns, conn)
	}
	body := make([]byte, chunk)
	if _, err := rand.Read(body); err != nil {
		return r, err
	}
	var verified, failures atomic.Uint64
	var wg sync.WaitGroup
	var sm *sampler
	if len(pids) > 0 {
		sm = &sampler{roots: pids, last: map[string]float64{}, start: time.Now().UnixMilli()}
		sm.sample()
	}
	start := time.Now()
	r.StartedAt = start.UTC()
	deadline := start.Add(duration)
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	stopped := make(chan struct{})
	if sm != nil {
		go func() {
			defer close(stopped)
			tick := time.NewTicker(250 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					sm.sample()
				case <-ctx.Done():
					sm.sample()
					return
				}
			}
		}()
	} else {
		close(stopped)
	}
	for _, conn := range conns {
		wg.Go(func() {
			read := make([]byte, chunk)
			stop := context.AfterFunc(ctx, func() { conn.Close() })
			defer stop()
			for ctx.Err() == nil {
				conn.SetDeadline(minTime(deadline, time.Now().Add(ioTimeout)))
				_, err := io.CopyN(conn, bytes.NewReader(body), int64(len(body)))
				if err == nil {
					_, err = io.ReadFull(conn, read)
				}
				if err != nil {
					if ctx.Err() == nil && time.Now().Before(deadline) {
						failures.Add(1)
					}
					return
				}
				if !bytes.Equal(body, read) {
					failures.Add(1)
					return
				}
				verified.Add(uint64(len(body)))
			}
		})
	}
	wg.Wait()
	r.Seconds = time.Since(start).Seconds()
	cancel()
	<-stopped
	r.VerifiedBytes = verified.Load()
	r.Failures = failures.Load()
	if r.Seconds > 0 {
		r.Mbps = float64(r.VerifiedBytes) * 8 / r.Seconds / 1e6
	}
	if sm != nil {
		if r.Seconds > 0 {
			sm.result.CPUPercent = sm.result.CPUSeconds / r.Seconds * 100
		}
		r.Resources = &sm.result
	}
	if parent.Err() != nil {
		return r, parent.Err()
	}
	return r, nil
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func serve(ctx context.Context, l net.Listener) error { return serveMode(ctx, l, false) }

func serveMode(ctx context.Context, l net.Listener, bulk bool) error {
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	slots := make(chan struct{}, 512)
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		wg.Go(func() {
			defer func() { conn.Close(); <-slots }()
			stop := context.AfterFunc(ctx, func() { conn.Close() })
			defer stop()
			conn.SetDeadline(time.Now().Add(24 * time.Hour))
			if bulk {
				serveBulk(conn)
			} else {
				_, _ = io.Copy(conn, conn)
			}
		})
	}
}
func run(ctx context.Context, args []string, out, diagnostics io.Writer) error {
	flags := flag.NewFlagSet("tunnelbench", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	var targets, pidArgs stringsFlag
	flags.Var(&targets, "target", "repeat name=host:port for each already configured TCP tunnel")
	flags.Var(&pidArgs, "pid", "repeat name=PID to sample the local engine and all helper descendants")
	listen := flags.String("serve", "", "run an echo backend, e.g. 127.0.0.1:9001")
	mode := flags.String("mode", "echo", "echo, bulk or soak; soak uses an echo backend")
	warmup := flags.Duration("warmup", 2*time.Second, "bulk warmup excluded from throughput and resource measurements (0..30s)")
	duration := flags.Duration("duration", 15*time.Second, "measurement time per target/concurrency")
	concurrency := flags.String("parallel", "1,8,32", "comma-separated simultaneous connections")
	chunk := flags.Int("chunk", 32<<10, "verified bytes per echo exchange")
	ioTimeout := flags.Duration("io-timeout", 2*time.Second, "maximum time for one verified echo exchange")
	asJSON := flags.Bool("json", false, "write a JSON array for comparison")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if (*mode != "echo" && *mode != "bulk" && *mode != "soak") || *warmup < 0 || *warmup > 30*time.Second {
		return errors.New("mode must be echo, bulk or soak; warmup must be 0..30s")
	}
	if *listen != "" {
		if len(targets) > 0 || len(pidArgs) > 0 {
			return errors.New("serve and measurement flags cannot be combined")
		}
		l, err := net.Listen("tcp", *listen)
		if err != nil {
			return err
		}
		defer l.Close()
		fmt.Fprintln(diagnostics, "Echo backend:", l.Addr())
		return serveMode(ctx, l, *mode == "bulk")
	}
	if len(targets) == 0 || len(targets) > 16 || *duration < time.Second || *duration > time.Hour || *chunk < 1 || *chunk > 1<<20 || *ioTimeout < 100*time.Millisecond || *ioTimeout > time.Minute {
		return errors.New("provide 1..16 targets; duration 1s..1h; chunk 1..1048576; io-timeout 100ms..1m")
	}
	var levels []int
	if len(strings.Split(*concurrency, ",")) > 8 {
		return errors.New("at most eight concurrency levels")
	}
	for _, raw := range strings.Split(*concurrency, ",") {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 256 {
			return errors.New("parallel must contain integers in 1..256")
		}
		levels = append(levels, n)
	}
	endpoints := map[string]string{}
	for _, target := range targets {
		name, address, ok := strings.Cut(target, "=")
		if !ok || name == "" || len(name) > 64 || strings.ContainsAny(name, "\r\n\t") || endpoints[name] != "" {
			return errors.New("targets must have unique names: name=host:port")
		}
		if _, _, err := net.SplitHostPort(address); err != nil {
			return err
		}
		endpoints[name] = address
	}
	pids := map[string][]int32{}
	for _, raw := range pidArgs {
		name, value, ok := strings.Cut(raw, "=")
		pid, err := strconv.ParseInt(value, 10, 32)
		if !ok || err != nil || pid < 1 || endpoints[name] == "" {
			return errors.New("pid must be an existing target name=positive-PID")
		}
		pids[name] = append(pids[name], int32(pid))
	}
	results := make([]result, 0, len(targets)*len(levels))
	for _, target := range targets {
		name, _, _ := strings.Cut(target, "=")
		for _, n := range levels {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintf(diagnostics, "Testing %s with %d connections...\n", name, n)
			if *mode == "soak" {
				if n != 1 {
					return errors.New("soak uses --parallel 1; each probe opens a fresh connection")
				}
				r, err := runSoak(ctx, endpoints[name], *duration, *ioTimeout, *chunk, pids[name])
				r.Name = name
				results = append(results, r)
				if err != nil {
					_ = json.NewEncoder(out).Encode(results)
					return fmt.Errorf("%s: %w", name, err)
				}
			} else if *mode == "bulk" {
				for _, direction := range []byte{'D', 'U'} {
					r, err := runBulkCase(ctx, endpoints[name], n, *duration, *warmup, *ioTimeout, direction, pids[name])
					if err != nil {
						return fmt.Errorf("%s %c: %w", name, direction, err)
					}
					r.Name = name
					results = append(results, r)
				}
			} else {
				r, err := runCase(ctx, endpoints[name], n, *duration, *chunk, *ioTimeout, pids[name])
				if err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				r.Name = name
				results = append(results, r)
			}
		}
	}
	if *asJSON {
		return json.NewEncoder(out).Encode(results)
	}
	fmt.Fprintln(out, "NAME CONNS MODE/DIRECTION VERIFIED-Mbps FAILURES CPU%(100=1core) PEAK-RSS-MiB")
	for _, r := range results {
		if r.Soak != nil {
			fmt.Fprintf(out, "%s soak %.1fs: %d/%d verified probes; %.2f%% unanswered; RTT p95 %.2f ms; jitter %.2f ms; recovery max %.2fs; complete=%t\n", r.Name, r.Seconds, r.Soak.Received, r.Soak.Attempted, r.Soak.LossPercent, r.Soak.P95RTTMillis, r.Soak.JitterMillis, r.Soak.MaxRecoverySeconds, r.Soak.Complete)
			continue
		}
		cpu, rss := "unmeasured", "unmeasured"
		if r.Resources != nil && r.Resources.Samples > 0 && r.Resources.Errors == 0 {
			cpu = fmt.Sprintf("%.1f", r.Resources.CPUPercent)
			rss = fmt.Sprintf("%.1f", float64(r.Resources.PeakRSS)/(1<<20))
		}
		label, mbps := "echo", r.Mbps
		if r.Mode == "bulk" {
			label, mbps = r.Mode+"/"+r.Direction, r.BulkMbps
		}
		fmt.Fprintf(out, "%s %d %s %.2f %d %s %s\n", r.Name, r.Parallel, label, mbps, r.Failures, cpu, rss)
	}
	return nil
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
