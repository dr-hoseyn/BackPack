package kernel

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"

	"github.com/backpack/backpack/config"
)

func rawBridge(ctx context.Context, c config.KernelConfig, local, remote string, ready func()) error {
	proto, header := "ip4:47", 8
	if c.Method == "etherip" {
		proto, header = "ip4:97", 2
	}
	socket, err := net.ListenPacket(proto, local)
	if err != nil {
		return err
	}
	defer socket.Close()
	dev, err := openDevice(c.Interface, true)
	if err != nil {
		return err
	}
	defer dev.Close()
	if ready != nil {
		ready()
	}
	stop := context.AfterFunc(ctx, func() { _ = socket.Close(); _ = dev.Close() })
	defer stop()
	var wg sync.WaitGroup
	result := make(chan error, 2)
	defer func() { _ = socket.Close(); _ = dev.Close(); wg.Wait() }()
	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := make([]byte, 16384)
		for {
			n, e := dev.Read(buf[header:])
			if e != nil {
				result <- e
				return
			}
			if header == 8 {
				binary.BigEndian.PutUint16(buf[:2], 0x2001)
				binary.BigEndian.PutUint16(buf[2:4], 0x6400)
				binary.BigEndian.PutUint16(buf[4:6], uint16(n))
				binary.LittleEndian.PutUint16(buf[6:8], uint16(c.ID))
			} else {
				buf[0], buf[1] = 0x30, 0
			}
			if _, e = socket.WriteTo(buf[:header+n], &net.IPAddr{IP: net.ParseIP(remote)}); e != nil {
				result <- e
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, 16384)
		for {
			n, src, e := socket.ReadFrom(buf)
			if e != nil {
				result <- e
				return
			}
			if src.String() != remote || n < header+14 {
				continue
			}
			if header == 8 {
				if binary.BigEndian.Uint16(buf[:2]) != 0x2001 || binary.BigEndian.Uint16(buf[2:4]) != 0x6400 || binary.LittleEndian.Uint16(buf[6:8]) != uint16(c.ID) || int(binary.BigEndian.Uint16(buf[4:6])) != n-header {
					continue
				}
			} else if buf[0] != 0x30 || buf[1] != 0 {
				continue
			}
			if _, e = dev.Write(buf[header:n]); e != nil {
				result <- e
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
		return nil
	case e := <-result:
		return fmt.Errorf("kernel raw Ethernet carrier: %w", e)
	}
}
