package config

// DNSConfig controls the direct DNS carrier. It sends complete DNS queries
// directly to the peer, with authenticated tunnel packets in an EDNS0 option;
// it does not use a recursive resolver or need an authoritative domain.
type DNSConfig struct {
	// DNSBind is the local literal IP to receive DNS packets on; empty takes
	// the listener address, or a wildcard address when dialing. Ports come
	// from addr and paths. It is local and is never copied into a setup link.
	DNSBind string `toml:"dns_bind"`
	// DNSRotate creates a fresh sending socket every N packets. Zero keeps
	// one ephemeral source port. Each endpoint may tune its sending side.
	DNSRotate int `toml:"dns_rotate"`
	// DNSRate caps outgoing DNS packets per second, including FEC and
	// handshakes. Zero disables the cap. It is independent of bandwidth_mbps.
	DNSRate int `toml:"dns_rate"`
}
