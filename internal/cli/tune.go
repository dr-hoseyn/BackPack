package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/backpack/backpack/internal/manage"
	"github.com/backpack/backpack/internal/optimize"
)

var (
	enableHighConcurrency  = optimize.EnableHighConcurrency
	restoreHighConcurrency = optimize.RestoreHighConcurrency
	tuneMSS                = func(name string, mss int) error { return manage.SetMSS(name, mss) }
)

const tuneUsage = `usage:
  backpack tune high-concurrency [--reserve-ports 62050,62051,20000-20010]
  backpack tune restore
  backpack tune path-mtu <tunnel-name> <measured-ipv4-mtu>

High concurrency reserves existing reservations, TCP listeners and configured
tunnel ports before widening the ephemeral range to 10240-65535. Include fixed
UDP services and stopped services with --reserve-ports. Existing tunnels and
the auto-refresh schedule keep running.

path-mtu uses the existing per-tunnel TCP MSS setting (MTU minus 52 bytes for
IPv4/TCP with timestamps). Apply on both ends; it restarts the named tunnel.
Use 0 to clear the MSS clamp. It does not change any host route or interface.
`

func runTune(args []string) Result {
	if len(args) == 0 {
		return fail(CodeUsage, "%s", tuneUsage)
	}
	if args[0] == "--help" || args[0] == "-h" {
		return ok(tuneUsage)
	}
	switch args[0] {
	case "high-concurrency":
		var extra []int
		for i := 1; i < len(args); i++ {
			a := args[i]
			var value string
			if a == "--reserve-ports" && i+1 < len(args) {
				i++
				value = args[i]
			} else if strings.HasPrefix(a, "--reserve-ports=") {
				value = strings.TrimPrefix(a, "--reserve-ports=")
			} else {
				return fail(CodeUsage, "%s", tuneUsage)
			}
			ports, err := optimize.ParseReservedPorts(value)
			if err != nil {
				return fail(CodeUsage, "%v\n", err)
			}
			extra = append(extra, ports...)
		}
		if !isRoot() {
			return fail(CodeFailed, "host tuning requires root (sudo)\n")
		}
		extra = append(extra, manage.ReservedPorts()...)
		reserved, err := enableHighConcurrency(extra)
		if err != nil {
			return fail(CodeFailed, "high-concurrency tuning failed: %v\n", err)
		}
		return ok(fmt.Sprintf("Ephemeral range: %s; reserved ports: %s\nApplied live and saved for boot. Tunnels and restart schedule unchanged.\n", optimize.HighConcurrencyRange, reserved))
	case "restore":
		if len(args) != 1 {
			return fail(CodeUsage, "%s", tuneUsage)
		}
		if !isRoot() {
			return fail(CodeFailed, "host tuning requires root (sudo)\n")
		}
		if err := restoreHighConcurrency(); err != nil {
			return fail(CodeFailed, "restore failed: %v\n", err)
		}
		return ok("Original port range and reservations restored. Restart schedule unchanged.\n")
	case "path-mtu":
		if len(args) != 3 {
			return fail(CodeUsage, "%s", tuneUsage)
		}
		mtu, err := strconv.Atoi(args[2])
		if err != nil || (mtu != 0 && (mtu < 576 || mtu > 1500)) {
			return fail(CodeUsage, "measured IPv4 MTU must be 576-1500, or 0 to clear the clamp\n")
		}
		if !isRoot() {
			return fail(CodeFailed, "editing a tunnel requires root (sudo)\n")
		}
		mss := 0
		if mtu != 0 {
			mss = mtu - 52
		}
		if err := tuneMSS(args[1], mss); err != nil {
			return fail(CodeFailed, "path MTU setting failed: %v\n", err)
		}
		return ok(fmt.Sprintf("%s: TCP MSS %d saved and tunnel restarted. Apply the measured limit on the other end too.\n", args[1], mss))
	default:
		return fail(CodeUsage, "%s", tuneUsage)
	}
}
