package manage

import "github.com/backpack/backpack/config"

func economyPathPolicy() config.PathPolicy {
	return config.PathPolicy{ProbeBytes: 1024, StandbyInterval: 60}
}

// Prefer one native datagram path and two independent TCP implementations.
// The server and client run only these selected carriers, bounding helpers on
// both ends. Full mode retains all eligible carriers and the existing policy.
func economicalCarriers(available []string) []string {
	var selected []string
	for _, candidate := range []string{"hysteria2", "reality", "anytls", "xhttp", "naive", "shadowtls", "short-https", "tuic"} {
		for _, v := range available {
			if v == candidate {
				selected = append(selected, v)
				break
			}
		}
		if len(selected) == 3 {
			break
		}
	}
	return selected
}
