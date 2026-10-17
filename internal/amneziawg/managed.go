package amneziawg

import (
	"context"
	"io"
	"strconv"
	"strings"
	"time"
)

// RuntimeStats contains only public runtime state. IpcGet also contains keys;
// its raw response must never enter logs, JSON APIs or the metrics snapshot.
type RuntimeStats struct {
	BytesIn, BytesOut uint64
	Endpoint          string
	LastHandshake     time.Time
}

func (s RuntimeStats) Connected() bool {
	return !s.LastHandshake.IsZero() && time.Since(s.LastHandshake) >= 0 && time.Since(s.LastHandshake) < 3*time.Minute
}

func runtimeStats(ipc string) RuntimeStats {
	var s RuntimeStats
	for _, line := range strings.Split(ipc, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "rx_bytes":
			s.BytesIn, _ = strconv.ParseUint(v, 10, 64)
		case "tx_bytes":
			s.BytesOut, _ = strconv.ParseUint(v, 10, 64)
		case "endpoint":
			s.Endpoint = v
		case "last_handshake_time_sec":
			n, err := strconv.ParseInt(v, 10, 64)
			if err == nil && n > 0 {
				s.LastHandshake = time.Unix(n, 0)
			}
		}
	}
	return s
}

// RunManaged is the same exclusive TUN engine as the standalone command,
// with a bounded public telemetry callback for the normal management process.
func RunManaged(ctx context.Context, c Config, out io.Writer, report func(RuntimeStats)) error {
	return runDeviceWithStats(ctx, c, out, report)
}
