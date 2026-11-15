package direct

import "net"

// Windows has no TCP_MAXSEG option. MSS tuning is best effort, matching Unix.
func clampMSS(tcp *net.TCPConn, mss int) {}
