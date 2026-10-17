package network

import "net"

// MPTCPStatus reports negotiation, not the number of available subflows. Extra
// paths are managed by Linux's path manager, outside this process.
func MPTCPStatus(c *net.TCPConn) string {
	on, err := c.MultipathTCP()
	if err != nil {
		return "MPTCP negotiation unknown"
	}
	if on {
		return "MPTCP negotiated"
	}
	return "ordinary TCP (MPTCP fallback)"
}
