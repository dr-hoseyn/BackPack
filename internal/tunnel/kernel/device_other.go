//go:build !linux

package kernel

import (
	"fmt"
	"os"
)

func lock(string) (func(), error) {
	return nil, fmt.Errorf("kernel tunnels require Linux and CAP_NET_ADMIN")
}
func openDevice(string, bool) (*os.File, error) {
	return nil, fmt.Errorf("kernel tunnels require Linux TUN/TAP")
}
