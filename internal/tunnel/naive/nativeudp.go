package naive

import (
	"errors"

	"github.com/backpack/backpack/config"
)

func validateNativeUDP(protocol string, enabled bool, flows int) error {
	if !enabled {
		if flows != 0 {
			return errors.New("udp_max_flows requires native_udp")
		}
		return nil
	}
	if !config.SingDatagram(protocol) {
		return errors.New("native reverse UDP requires Hysteria2 or TUIC")
	}
	if flows < 0 || flows > 4096 {
		return errors.New("native UDP requires 0..4096 flows")
	}
	return nil
}
