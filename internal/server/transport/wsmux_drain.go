package transport

import (
	"context"
	"io"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

// wsMuxDrain keeps the scheduler's hot path independent of filesystem IO.
// When the operator removes the file, all carriers resume normal admission.
type wsMuxDrain struct {
	peers atomic.Pointer[map[netip.AddrPort]struct{}]
}

func (d *wsMuxDrain) forPeer(remote string) func() bool {
	peer, err := netip.ParseAddrPort(remote)
	if err != nil {
		return nil
	}
	return func() bool {
		peers := d.peers.Load()
		if peers == nil {
			return false
		}
		_, marked := (*peers)[peer]
		return marked
	}
}

func (d *wsMuxDrain) watch(ctx context.Context, path string, log *logrus.Logger) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			d.reload(path, log)
		}
	}
}

func (d *wsMuxDrain) reload(path string, log *logrus.Logger) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		d.peers.Store(nil)
		return
	}
	if err != nil {
		log.Debugf("read WSSMux drain peers: %v", err)
		return
	}
	if !info.Mode().IsRegular() {
		return
	}
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	const maxSize = 16 << 10
	body, err := io.ReadAll(io.LimitReader(file, maxSize+1))
	if err != nil || len(body) > maxSize {
		log.Debug("invalid WSSMux drain peers file")
		return
	}
	peers := make(map[netip.AddrPort]struct{})
	for _, addr := range strings.Fields(string(body)) {
		peer, err := netip.ParseAddrPort(addr)
		if err != nil {
			log.Debugf("invalid WSSMux drain peer: %q", addr)
			return
		}
		peers[peer] = struct{}{}
	}
	d.peers.Store(&peers)
}
