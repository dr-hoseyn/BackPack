package config

// HysteriaRealm describes an existing rendezvous service. STUNServers is a
// comma-separated explicit list; no third-party service is silently selected.
// Token is a separate rendezvous credential, never the reverse tunnel token.
type HysteriaRealm struct {
	// ServerURL is the HTTPS rendezvous endpoint with normal system trust.
	ServerURL string `toml:"server_url,omitempty" json:"server_url,omitempty"`
	// Token authenticates this registration independently of the tunnel.
	Token string `toml:"token,omitempty" json:"token,omitempty"`
	// RealmID identifies the shared 1..64 character slot.
	RealmID string `toml:"realm_id,omitempty" json:"realm_id,omitempty"`
	// STUNServers lists one to eight explicit host:port endpoints, comma-separated.
	STUNServers string `toml:"stun_servers,omitempty" json:"stun_servers,omitempty"`
	// IPVersion restricts discovery to 4 or 6; zero allows either.
	IPVersion int `toml:"ip_version,omitempty" json:"ip_version,omitempty"`
}

func (r HysteriaRealm) Enabled() bool { return r != (HysteriaRealm{}) }
