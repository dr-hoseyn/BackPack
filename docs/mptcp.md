# Multipath TCP

Native reverse `tcp`, `stealth` and `tcpmux` tunnels can request Linux MPTCP
with `mptcp = true` in their `[server]` and `[client]` tables. It affects the
tunnel sockets only; backend connections and forwarded entry sockets retain
ordinary TCP. Existing configurations do not request MPTCP.

Both machines need a kernel supporting MPTCP for negotiation to succeed. Go
falls back to ordinary TCP when the kernel or peer does not support it. The
server's debug log records the actual negotiated result for each tunnel socket.
An MPTCP connection can still have only one subflow: additional independent
interfaces, addresses and Linux path-manager configuration are required to
gain path diversity. BackPack does not change routing, sysctls or endpoints.

Managed helpers, outbound proxies and other transports reject `mptcp`: a
setting on their loopback connection would not enable MPTCP on the Internet
connection that the helper owns. The setting is preserved by normal edits.

Verify negotiation and sustained data on the actual path. Then remove one
configured path while traffic is flowing and verify the remaining subflow.
MPTCP is not a reachability workaround for a destination blocked on every path.
