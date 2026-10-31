package kernel

import (
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/backpack/backpack/config"
)

// Command never includes a private key in argv or in an error. WireGuard's
// syncconf reads its secret through stdin; IPsec credentials use local VICI.
type Command func(context.Context, string, []string, io.Reader) ([]byte, error)

func command(ctx context.Context, name string, args []string, input io.Reader) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = input
	out, err := cmd.CombinedOutput()
	if err != nil && name == "wg" {
		return nil, fmt.Errorf("kernel: wg configuration failed: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("kernel: %s failed: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func execute(ctx context.Context, p Plan, run Command) (cleanup func() error, err error) {
	var undo [][]string
	cleanup = func() error {
		clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		var failures []error
		for i := len(undo) - 1; i >= 0; i-- {
			if _, e := run(clean, "ip", undo[i], nil); e != nil {
				failures = append(failures, e)
			}
		}
		undo = nil
		return errors.Join(failures...)
	}
	for _, name := range p.Interfaces {
		out, e := run(ctx, "ip", []string{"-j", "link", "show", "dev", name}, nil)
		if e == nil {
			var links []struct {
				Alias string `json:"ifalias"`
			}
			if json.Unmarshal(out, &links) != nil || len(links) != 1 || links[0].Alias != owner(p.Config) {
				return cleanup, fmt.Errorf("kernel: interface %s already exists and is not owned by this tunnel", name)
			}
			if _, e = run(ctx, "ip", []string{"link", "del", name}, nil); e != nil {
				return cleanup, e
			}
		}
	}
	for _, step := range p.Steps {
		if _, err = run(ctx, "ip", step.Args, nil); err != nil {
			_ = cleanup()
			return cleanup, err
		}
		if len(step.Undo) > 0 {
			undo = append(undo, step.Undo)
		}
	}
	return cleanup, nil
}

func wireguardConfig(c config.KernelConfig) string {
	key := func(role string) *ecdh.PrivateKey {
		s := sha256.Sum256([]byte("backpack-kernel-wg-v1|" + c.Token + "|" + role))
		k, _ := ecdh.X25519().NewPrivateKey(s[:])
		return k
	}
	other := "iran"
	if c.Role == "iran" {
		other = "kharej"
	}
	psk := sha256.Sum256([]byte("backpack-kernel-wg-psk-v1|" + c.Token))
	text := fmt.Sprintf("[Interface]\nPrivateKey = %s\nListenPort = %d\n[Peer]\nPublicKey = %s\nPresharedKey = %s\nAllowedIPs = 0.0.0.0/0, ::/0\n", base64.StdEncoding.EncodeToString(key(c.Role).Bytes()), c.Port, base64.StdEncoding.EncodeToString(key(other).PublicKey().Bytes()), base64.StdEncoding.EncodeToString(psk[:]))
	if !c.Reverse || c.Role == "kharej" {
		text += fmt.Sprintf("Endpoint = %s\nPersistentKeepalive = 20\n", net.JoinHostPort(c.RemoteAddr, number(c.Port)))
	}
	return text
}

// Run owns one native tunnel generation. Its lock outlives teardown. A new
// process can recover tagged interfaces after a crash, but cannot delete a
// running generation or an operator's interface that happens to share a name.
func Run(ctx context.Context, input config.KernelConfig) error { return RunManaged(ctx, input, nil) }

// RunManaged reports local data-plane readiness for guarded configuration edits.
// Readiness does not claim that the remote endpoint is reachable.
func RunManaged(ctx context.Context, input config.KernelConfig, ready func(bool)) (err error) {
	if ready != nil {
		ready(false)
		defer ready(false)
	}
	p, err := Build(input)
	if err != nil {
		return err
	}
	c := p.Config
	release, err := lock(c.Interface)
	if err != nil {
		return err
	}
	defer release()
	clean, err := execute(ctx, p, command)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, clean()) }()
	if c.Method == "wireguard" {
		if _, err = command(ctx, "wg", []string{"syncconf", c.Interface, "/dev/stdin"}, strings.NewReader(wireguardConfig(c))); err != nil {
			return err
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var workers sync.WaitGroup
	failures := make(chan error, 3)
	started := make(chan struct{}, 2)
	pending := 0
	signalReady := func() { started <- struct{}{} }
	start := func(f func() error) { workers.Add(1); go func() { defer workers.Done(); failures <- f() }() }
	// End and join every device reader, socket and forwarder before deleting
	// interfaces or allowing the configuration reload to start its replacement.
	defer func() { cancel(); workers.Wait() }()
	if c.Reverse && c.Method != "wireguard" {
		pending++
		start(func() error { return reverse(runCtx, c, signalReady) })
	}
	if c.Method == "eoip" || c.Method == "etherip" {
		pending++
		start(func() error { return rawBridge(runCtx, c, p.Local, p.Remote, signalReady) })
	}
	if c.Method == "xfrm-ipsec" {
		stop, e := ipsec(runCtx, c, p.Local, p.Remote)
		if e != nil {
			return e
		}
		defer func() {
			clean, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, _ = command(clean, "ip", []string{"link", "set", "dev", c.Interface, "down"}, nil)
			stop()
		}()
		if _, e = command(runCtx, "ip", []string{"link", "set", "dev", c.Interface, "up"}, nil); e != nil {
			return e
		}
	}
	for i := 0; i < pending; i++ {
		select {
		case <-started:
		case e := <-failures:
			return e
		case <-ctx.Done():
			return nil
		}
	}
	forwarder, err := Forwarder(c)
	if err != nil {
		return err
	}
	if forwarder != nil {
		start(func() error { return forwarder.Run(runCtx) })
	}
	if ready != nil {
		ready(true)
	}
	select {
	case <-ctx.Done():
		return nil
	case e := <-failures:
		if ctx.Err() != nil {
			return nil
		}
		return e
	}
}

// Counters are cumulative interface bytes. Missing counters during creation or
// teardown are tolerated by the collector, rather than presented as errors.
func Counters(iface string) (uint64, uint64) {
	read := func(name string) uint64 {
		b, e := os.ReadFile(filepath.Join("/sys/class/net", iface, "statistics", name))
		if e != nil {
			return 0
		}
		n, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		return n
	}
	return read("rx_bytes"), read("tx_bytes")
}
