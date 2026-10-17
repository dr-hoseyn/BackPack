//go:build !linux

package amneziawg

import (
	"context"
	"errors"
	"io"
)

func runDevice(context.Context, Config, io.Writer) error {
	return errors.New("the AmneziaWG prototype runs on Linux with TUN and CAP_NET_ADMIN; init and check work on this platform")
}
