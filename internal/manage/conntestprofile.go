package manage

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	ConnTestQuick        = "quick"
	ConnTestFull         = "full"
	connTestQuickSamples = 20
	ctScreened           = "screened"
)

// Empty retains the full test for existing callers and older test links.
func ConnTestMode(mode string) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = ConnTestFull
	}
	if mode != ConnTestQuick && mode != ConnTestFull {
		return "", fmt.Errorf("test mode must be quick or full")
	}
	return mode, nil
}

func (c *connTestCase) samples() int {
	if c.mode == ConnTestQuick {
		return connTestQuickSamples
	}
	return connTestSoak
}

func ctEchoContext(ctx context.Context, conn net.Conn, udp bool) error {
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := ctx.Err(); err != nil {
		return err
	}
	return ctEcho(conn, udp)
}

// Estimates include the serial speed budget, not just the sampling window.
func ConnTestRunTimeFor(mode string) time.Duration {
	if mode == ConnTestQuick {
		return 30*time.Second + connTestQuickSamples*time.Second + 20*time.Second + time.Duration(len(connTestReverse)-1+len(ctDirectPlans("")))*ctQuickSpeedProfile.budget()*2
	}
	return connTestConnectWait + time.Duration(connTestSoak)*time.Second + 45*time.Second + ConnTestSpeedTime(len(connTestReverse)-1+len(ctDirectPlans("")))
}
