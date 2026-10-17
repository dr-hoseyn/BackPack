package client

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/backpack/backpack/internal/nativeudp"
	"github.com/backpack/backpack/internal/tunnel/naive"
)

func (c *Client) startNativeUDP(proxyURL string) func() {
	ctx, cancel := context.WithCancel(c.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			pc, err := naive.NewSOCKSPacketConn(ctx, strings.TrimPrefix(proxyURL, "socks5://"), c.config.RemoteAddr)
			if err == nil {
				peer, _ := net.ResolveUDPAddr("udp", c.config.RemoteAddr)
				e, startErr := nativeudp.StartClient(ctx, pc, peer, c.config.Token, c.config.Sing.UDPMaxFlows, c.config.Sing.UDPBandwidthMbps)
				if startErr == nil {
					<-ctx.Done()
					e.Close()
					return
				}
				pc.Close()
				err = startErr
			}
			if ctx.Err() != nil {
				return
			}
			c.logger.Warnf("native reverse UDP association unavailable: %v; retrying", err)
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (c *Client) startNativePacket(pc net.PacketConn, peer net.Addr) (func(), error) {
	native := c.config.NativeSing()
	e, err := nativeudp.StartClient(c.ctx, pc, peer, c.config.Token, native.UDPMaxFlows, native.UDPBandwidthMbps)
	if err != nil {
		pc.Close()
		return nil, err
	}
	return func() { e.Close() }, nil
}
