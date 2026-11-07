package network

import (
	"context"
	"time"

	"github.com/backpack/backpack/config"
)

// WSProbePath uses the normal tunnel authentication without claiming the
// control seat or occupying a data slot.
const (
	WSProbePath      = "/probe"
	WSProbeAckHeader = "X-Backpack-Probe"
)

// WebSocketHealthProbe measures a complete authenticated upgrade through the
// same proxy, edge, SNI and trust settings as a real tunnel dial.
func WebSocketHealthProbe(out *Outbound, edgeIP, token string, mode config.TransportType, simpleAuth bool, mss int, options *WSSClientOptions) func(context.Context, string, int) EndpointScore {
	return func(ctx context.Context, addr string, samples int) EndpointScore {
		// Give a blackholed address one budget for the entire batch. At most
		// three handshakes are needed to distinguish a slow path from a loss.
		ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		return scoreAttempts(ctx, addr, max(1, min(samples, 3)), func() bool {
			conn, err := WebSocketDialer(ctx, out, addr, edgeIP, WSProbePath, 2*time.Second, 0, true, token, mode, simpleAuth, 1, 0, 0, mss, options)
			if err != nil {
				return false
			}
			conn.Close()
			return true
		})
	}
}
