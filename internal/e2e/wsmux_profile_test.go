package e2e

import "github.com/backpack/backpack/config"

// A reproducible 600 Mbit/s candidate for a path with RTT up to about 200 ms.
// The bandwidth-delay product of one 600 Mbit/s stream is about 15 MB. This
// profile is opt-in; throughput still depends on CPU and the actual path.
func configureBoundedWSMux(s *config.ServerConfig, c *config.ClientConfig) {
	s.MuxVersion, c.MuxVersion = 2, 2
	s.MuxCon, c.ConnectionPool, c.WSMuxMaxSessions = 128, 8, 16
	s.MaxFrameSize, c.MaxFrameSize = 32768, 32768
	s.MaxReceiveBuffer, c.MaxReceiveBuffer = 32<<20, 32<<20
	s.MaxStreamBuffer, c.MaxStreamBuffer = 16<<20, 16<<20
	s.Nodelay, c.Nodelay = true, true
	c.AggressivePool = false
}
