package e2e

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

// Download capacity is measured separately from echo: echo asks the same test
// host to carry the target rate in both directions at once. Count only payload
// actually received and verified, with no doubling of a one-direction result.
func TestBulkProfileDownloadCapacity(t *testing.T) {
	if os.Getenv("BP_CAPACITY_BENCHMARK") == "" {
		t.Skip("set BP_CAPACITY_BENCHMARK=1 to measure")
	}
	var minimum float64
	if value := os.Getenv("BP_CAPACITY_MIN_MBPS"); value != "" {
		var err error
		minimum, err = strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(minimum) || math.IsInf(minimum, 0) || minimum <= 0 {
			t.Fatal("BP_CAPACITY_MIN_MBPS must be a finite positive application-goodput target")
		}
	}
	payload := randomPayload(t, 32<<20)
	for _, tr := range []string{"tcp", "stealth", "tcpmux", "ws", "wss", "wsmux", "wssmux", "kcp", "quic"} {
		t.Run(tr, func(t *testing.T) {
			backend, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { backend.Close() })
			go func() {
				for {
					c, err := backend.Accept()
					if err != nil {
						return
					}
					go func() {
						defer c.Close()
						c.SetDeadline(time.Now().Add(20 * time.Second))
						var command [1]byte
						if _, err := io.ReadFull(c, command[:]); err != nil {
							return
						}
						if command[0] == 0xff {
							io.Copy(struct{ io.Writer }{c}, bytes.NewReader(payload))
						} else {
							// The normal readiness probe still makes a real round trip.
							c.Write(command[:])
							io.Copy(struct{ io.Writer }{c}, struct{ io.Reader }{c})
						}
					}()
				}
			}()
			tun := bulkProfileForBackend(t, tr, backend.Addr().String(), nil)
			time.Sleep(time.Second)
			const streams = 8
			connections := make([]net.Conn, streams)
			for i := range connections {
				connections[i], err = net.DialTimeout("tcp", tun.Entry, 3*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer connections[i].Close()
				connections[i].SetDeadline(time.Now().Add(20 * time.Second))
			}
			results := make(chan error, streams)
			start := time.Now()
			for _, c := range connections {
				go func(c net.Conn) {
					if _, err := c.Write([]byte{0xff}); err != nil {
						results <- err
						return
					}
					got := make([]byte, len(payload))
					_, err := io.ReadFull(c, got)
					if err == nil && !bytes.Equal(got, payload) {
						err = fmt.Errorf("download payload corrupted")
					}
					results <- err
				}(c)
			}
			for range streams {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			seconds := time.Since(start).Seconds()
			goodput := float64(streams*len(payload)*8) / seconds / 1e6
			t.Logf("CAPACITY_DOWNLOAD transport=%s streams=%d goodput_mbps=%.1f seconds=%.3f", tr, streams, goodput, seconds)
			if minimum > 0 && goodput < minimum {
				t.Errorf("download %.1f Mbit/s is below requested %.1f Mbit/s", goodput, minimum)
			}
		})
	}
}
