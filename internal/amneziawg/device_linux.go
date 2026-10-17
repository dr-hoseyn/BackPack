//go:build linux

package amneziawg

import (
	"errors"
	"sync"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
)

// The pinned engine starts its TUN reader in NewDevice, before IpcSet can set
// S4. Its first Read can therefore capture a zero padding offset. Wake that
// initial read without consuming a packet after configuration; the next read
// uses the configured offset. No real packet is discarded or rewritten.
type configuredTUN struct {
	tun.Device
	ready chan struct{}
	once  sync.Once
	first bool
}

func (t *configuredTUN) release() { t.once.Do(func() { close(t.ready) }) }
func (t *configuredTUN) Read(packets [][]byte, sizes []int, offset int) (int, error) {
	if !t.first {
		t.first = true
		<-t.ready
		return 0, nil
	}
	return t.Device.Read(packets, sizes, offset)
}
func (t *configuredTUN) Close() error { t.release(); return t.Device.Close() }

func newConfiguredDevice(t tun.Device, ipc string, logger *device.Logger) (*device.Device, error) {
	wrapped := &configuredTUN{Device: t, ready: make(chan struct{})}
	d := device.NewDevice(wrapped, conn.NewDefaultBind(), logger)
	if err := d.IpcSet(ipc); err != nil {
		d.Close()
		return nil, errors.New("AmneziaWG rejected the bounded configuration")
	}
	wrapped.release()
	return d, nil
}
