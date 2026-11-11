package manage

import (
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ctPickPort is a random port free on this server (for TCP, and for UDP as
// well when both is set) and not already given to another part of the test.
// The kharej's echo ports are picked here too; they are random, so a clash
// there is unlikely, and a tunnel that meets one reports it.
//
// It stays below the kernel's ephemeral range. The port is free only when
// checked and is bound later by an engine, and meanwhile every outgoing
// connection on the server draws its source port from that range: one could
// take the number first, and the tunnel listening there would never come up.
// See internal/testport for the same failure in the test suites.
func ctPickPort(used map[int]bool, both bool) int {
	for i := 0; i < 500; i++ {
		var b [2]byte
		_, _ = rand.Read(b[:])
		p := 20000 + (int(b[0])<<8|int(b[1]))%(ephemeralDefaultLow-20000)
		if used[p] {
			continue
		}
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err != nil {
			continue
		}
		l.Close()
		if both {
			u, err := net.ListenPacket("udp", fmt.Sprintf(":%d", p))
			if err != nil {
				continue
			}
			u.Close()
		}
		used[p] = true
		return p
	}
	return 0
}

// ctFreeL3Block is a 10.10.N. block no tunnel on this server uses, from the
// top of the range the wizard counts up from, one per direct tunnel under
// test.
func ctFreeL3Block(i int) string {
	skip := i
	for n := 250; n > 0; n-- {
		block := fmt.Sprintf("10.10.%d.", n)
		if l3BlockOwner(block+"1") != "" {
			continue
		}
		if skip == 0 {
			return block
		}
		skip--
	}
	return fmt.Sprintf("10.10.%d.", 250-i)
}

// ctQuiet turns off the kernel tuning in a reverse config: it is for a
// tunnel that stays, and the tunnels here last two minutes.
func ctQuiet(body string) string {
	for _, table := range []string{"[server]\n", "[client]\n"} {
		body = strings.Replace(body, table, table+"skip_optz = true\n", 1)
	}
	return body
}

// ctEngine is one engine process under test.
type ctEngine struct {
	name string
	cmd  *exec.Cmd
	log  string
	done chan struct{}
}

func startCTEngine(dir, name, body string) (*ctEngine, error) {
	bin, err := connTestBinary()
	if err != nil {
		return nil, err
	}
	cfg := filepath.Join(dir, name+".toml")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		return nil, err
	}
	logPath := filepath.Join(dir, name+".log")
	f, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, "-c", cfg)
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, fmt.Errorf("starting the %s engine: %w", name, err)
	}
	e := &ctEngine{name: name, cmd: cmd, log: logPath, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		f.Close()
		close(e.done)
	}()
	return e, nil
}

// exited reports whether the engine has stopped on its own, and the last
// thing it said.
func (e *ctEngine) exited() (bool, string) {
	select {
	case <-e.done:
		return true, ctLastLine(e.log)
	default:
		return false, ""
	}
}

// ctStopAll stops engines the way systemd would — asked first, so each takes
// down its interface and firewall rules — and kills what is still there.
func ctStopAll(engines []*ctEngine) {
	for _, e := range engines {
		select {
		case <-e.done:
		default:
			_ = e.cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	deadline := time.NewTimer(connTestStopWait)
	defer deadline.Stop()
	expired := false
	for _, e := range engines {
		if expired {
			select {
			case <-e.done:
				continue
			default:
				_ = syscall.Kill(-e.cmd.Process.Pid, syscall.SIGKILL)
				<-e.done
				continue
			}
		}
		select {
		case <-e.done:
		case <-deadline.C:
			expired = true
			_ = syscall.Kill(-e.cmd.Process.Pid, syscall.SIGKILL)
			<-e.done
		}
	}
}

func ctLastLine(path string) string {
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
