package naive

import (
	"errors"

	"github.com/backpack/backpack/config"
)

func validateNativeUDP(protocol string, enabled bool, flows, mbps int) error {
	if !enabled {
		if flows != 0 || mbps != 0 {
			return errors.New("udp_max_flows and udp_bandwidth_mbps require native_udp")
		}
		return nil
	}
	if !config.SingDatagram(protocol) {
		return errors.New("native reverse UDP requires Hysteria2 or TUIC")
	}
	if flows < 0 || flows > 4096 || mbps < 0 || mbps > 100000 {
		return errors.New("native UDP limits require 0..4096 flows and 0..100000 Mbit/s")
	}
	return nil
}
