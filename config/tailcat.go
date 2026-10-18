package config

// TailcatConfig identifies a point-to-point peer through an explicitly selected
// DERP map. Listen/Server on the enclosing Sing config are pairing metadata;
// Tailcat opens no public TCP/UDP listener at that advertised endpoint.
type TailcatConfig struct {
	// PrivateKey is this peer's canonical base64 X25519 private key.
	PrivateKey string `toml:"private_key" json:"-"`
	// ClientKey provisions the one authorized client, server only. Setup links
	// carry this client's key; the server's own private key never leaves it.
	ClientKey string `toml:"client_key,omitempty" json:"-"`
	// PublicKey is the server's X25519 public key, client only.
	PublicKey string `toml:"public_key,omitempty"`
	// DiscoKey is the server's derived discovery public key, client only.
	DiscoKey string `toml:"disco_key,omitempty"`
	// PreSharedKey is a canonical base64 32-byte key shared by this pair.
	PreSharedKey string `toml:"pre_shared_key" json:"-"`
	// DERPMapURL explicitly chooses a verified HTTPS relay map; no public default is assumed.
	DERPMapURL string `toml:"derp_map_url"`
	// DERPRegion is the required region ID from the selected map, in 1..65535.
	DERPRegion int `toml:"derp_region,omitempty"`
}
