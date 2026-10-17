//go:build linux

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Running out of file descriptors, for real.
//
// This is the third of the faults in chaos_test.go and the one that cannot be
// simulated: the interesting part is that *every* syscall that needs a
// descriptor fails at once — accept, dial, open — while everything already open
// keeps working perfectly. A stub listener returning EMFILE tests the accept
// loop's backoff, which internal/utils/acceptloop already does. It does not test
// what a tunnel does.
//
// The limit is process-wide, so lowering it inside the ordinary test binary
// would break whatever else that binary is doing. The test therefore re-runs
// itself as a child process, lowers the limit there, and reports what the child
// found. That is also closer to the real thing: a VPS with a low `nofile` is
// one where the limit was low before the tunnel started.
//
// What it holds:
//
//   - the process does not die, and does not spin;
//   - a caller that arrives while there are no descriptors gets an error rather
//     than a connection that hangs for ever;
//   - when descriptors are free again, the tunnel carries traffic without
//     anybody restarting it.

const fdChaosEnv = "BACKPACK_FD_CHAOS_CHILD"

func TestATunnelSurvivesRunningOutOfFileDescriptors(t *testing.T) {
	if os.Getenv(fdChaosEnv) == "1" {
		fdChaosChild(t)
		return
	}
	if testing.Short() {
		t.Skip("re-runs this binary as a child process — skipped under -short")
	}

	// Keep the real service and the incoming caller outside the starved
	// process. A local Dial under EMFILE fails before it ever reaches Accept,
	// which used to leave the tunnel's recovery almost entirely unexercised.
	backend := startEchoBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^TestATunnelSurvivesRunningOutOfFileDescriptors$",
		"-test.v", "-test.timeout=4m")
	cmd.Env = append(os.Environ(), fdChaosEnv+"=1", "BACKPACK_FD_CHAOS_BACKEND="+backend.addr)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	reader, writer := io.Pipe()
	cmd.Stdout, cmd.Stderr = writer, writer
	ready := make(chan string, 1)
	output := make(chan string, 1)
	go func() {
		var out strings.Builder
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Fprintln(&out, line)
			if addr, ok := strings.CutPrefix(line, fdChaosMarker+": "); ok {
				ready <- addr
			}
		}
		output <- out.String()
	}()
	if err := cmd.Start(); err != nil {
		writer.Close()
		reader.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { err := cmd.Wait(); writer.Close(); done <- err }()
	select {
	case addr := <-ready:
		// TCP's backlog may accept the handshake even while Accept cannot
		// allocate a socket. The request must remain bounded, then the child
		// releases its descriptors and proves actual payload recovery.
		c, dialErr := net.DialTimeout("tcp", addr, 3*time.Second)
		if dialErr == nil {
			_ = c.SetDeadline(time.Now().Add(3 * time.Second))
			_, _ = c.Write([]byte("outside caller while descriptors are exhausted"))
			var reply [1]byte
			_, _ = c.Read(reply[:])
			c.Close()
		}
		_, _ = fmt.Fprintln(input, "release")
		err = <-done
	case err = <-done:
	}
	out := <-output
	if err != nil {
		t.Fatalf("the tunnel did not survive running out of file descriptors:\n%s", out)
	}
	// "PASS" alone is not proof: a child whose -test.run matched nothing prints
	// it too, and the whole test would then be a very elaborate no-op. The
	// marker is a line only the child's own body can print.
	if !strings.Contains(out, fdChaosMarker) {
		t.Fatalf("the child did not run the test — it printed:\n%s", out)
	}
}

// fdLimit is the ceiling the child runs under. Low enough that it can be
// reached in a moment, high enough that the Go runtime, the test binary and a
// tunnel all fit underneath it with room to work.
const fdLimit = 512

// fdChaosMarker is how the parent knows the child really ran.
const fdChaosMarker = "descriptors held with none left"

func fdChaosChild(t *testing.T) {
	var was unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &was); err != nil {
		t.Fatalf("reading the descriptor limit: %v", err)
	}
	if was.Max < fdLimit {
		t.Skipf("the hard descriptor limit is %d, below the %d this test needs", was.Max, fdLimit)
	}
	lowered := unix.Rlimit{Cur: fdLimit, Max: was.Max}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &lowered); err != nil {
		t.Fatalf("lowering the descriptor limit: %v", err)
	}
	t.Cleanup(func() { _ = unix.Setrlimit(unix.RLIMIT_NOFILE, &was) })

	backend := &echoBackend{addr: os.Getenv("BACKPACK_FD_CHAOS_BACKEND")}
	if backend.addr == "" {
		t.Fatal("the parent did not supply an independent backend")
	}
	tun := startTunnel(t, "tcp", backend, tunnelOptions{})
	if err := tun.roundTrip(randomPayload(t, 4096)); err != nil {
		t.Fatalf("the tunnel did not work before the descriptors ran out: %v", err)
	}

	held := exhaustFDs(t)
	defer releaseFDs(held)
	t.Logf("holding %d descriptors", len(held))
	if len(held) == 0 {
		t.Fatal("could not exhaust the descriptors, so nothing was tested")
	}

	fmt.Printf("%s: %s\n", fdChaosMarker, tun.Entry)
	var command string
	if _, err := fmt.Fscanln(os.Stdin, &command); err != nil || command != "release" {
		t.Fatalf("the outside caller did not finish its starvation probe: %v", err)
	}

	releaseFDs(held)

	if err := waitFor(60*time.Second, func() bool {
		return tun.roundTrip(randomPayload(t, 4096)) == nil
	}); err != nil {
		t.Fatalf("the tunnel never recovered after the descriptors were free again: %v", err)
	}
}

// exhaustFDs opens descriptors until the process cannot have any more, and
// returns the ones it is holding.
func exhaustFDs(t *testing.T) []*os.File {
	t.Helper()
	var held []*os.File
	for i := 0; i < fdLimit*2; i++ {
		f, err := os.Open(os.DevNull)
		if err != nil {
			break
		}
		held = append(held, f)
	}
	return held
}

func releaseFDs(held []*os.File) {
	for _, f := range held {
		_ = f.Close()
	}
}
