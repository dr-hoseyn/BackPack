package l3

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"time"

	"github.com/backpack/backpack/internal/httpcarrier"
)

type webTransportCarrier struct{ *httpcarrier.PacketConn }

func (*webTransportCarrier) Overhead() int       { return 192 }
func (*webTransportCarrier) CarrierName() string { return CarrierWebTransport }

func openWebTransport(ctx context.Context, cfg Config) (DatagramCarrier, net.Addr, error) {
	t := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: cfg.Masque.ServerName}
	sum := sha256.Sum256([]byte("backpack WebTransport datagrams v1\x00" + cfg.Token))
	password := hex.EncodeToString(sum[:])
	const path = "/.well-known/backpack/webtransport/datagram"
	if cfg.Mode == ModeListen {
		pair, err := tls.LoadX509KeyPair(cfg.Masque.CertFile, cfg.Masque.KeyFile)
		if err != nil {
			return nil, nil, err
		}
		t.Certificates = []tls.Certificate{pair}
		p, err := httpcarrier.ListenPackets(ctx, cfg.Addr, path, "backpack", password, t)
		if err != nil {
			return nil, nil, err
		}
		return &webTransportCarrier{p}, nil, nil
	}
	if t.ServerName == "" {
		t.ServerName, _, _ = net.SplitHostPort(cfg.Addr)
	}
	if cfg.Masque.CAFile != "" {
		b, err := os.ReadFile(cfg.Masque.CAFile)
		if err != nil {
			return nil, nil, err
		}
		t.RootCAs = x509.NewCertPool()
		if !t.RootCAs.AppendCertsFromPEM(b) {
			return nil, nil, errors.New("WebTransport CA file contains no certificates")
		}
	}
	setup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	p, err := httpcarrier.DialPackets(ctx, setup, cfg.Addr, path, "backpack", password, t)
	if err != nil {
		return nil, nil, err
	}
	return &webTransportCarrier{p}, p.Peer(), nil
}
