package l3

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"

	"github.com/backpack/backpack/internal/masqueh2"
)

type masqueH2Carrier struct{ *masqueh2.Conn }

// HTTP/2 is an emergency reliable carrier. The conservative budget includes
// IPv6/TCP/TLS/DATA and Capsule framing; TCP may split packets further.
func (*masqueH2Carrier) Overhead() int       { return 192 }
func (*masqueH2Carrier) CarrierName() string { return CarrierMasqueH2 }

func openMasqueH2(ctx context.Context, cfg Config) (DatagramCarrier, net.Addr, error) {
	if err := cfg.Masque.Validate(cfg.Mode == ModeListen); err != nil {
		return nil, nil, err
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: cfg.Masque.ServerName}
	if cfg.Mode == ModeListen {
		pair, err := tls.LoadX509KeyPair(cfg.Masque.CertFile, cfg.Masque.KeyFile)
		if err != nil {
			return nil, nil, err
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
		c, err := masqueh2.Listen(ctx, cfg.Addr, cfg.Masque.TargetAddr(), cfg.Token, tlsCfg)
		if err != nil {
			return nil, nil, err
		}
		return &masqueH2Carrier{c}, nil, nil
	}
	if tlsCfg.ServerName == "" {
		tlsCfg.ServerName, _, _ = net.SplitHostPort(cfg.Addr)
	}
	if cfg.Masque.CAFile != "" {
		pem, err := os.ReadFile(cfg.Masque.CAFile)
		if err != nil {
			return nil, nil, err
		}
		tlsCfg.RootCAs = x509.NewCertPool()
		if !tlsCfg.RootCAs.AppendCertsFromPEM(pem) {
			return nil, nil, errors.New("MASQUE/H2 CA file contains no certificates")
		}
	}
	c, err := masqueh2.Dial(ctx, cfg.Addr, cfg.Masque.TargetAddr(), cfg.Token, tlsCfg)
	if err != nil {
		return nil, nil, err
	}
	return &masqueH2Carrier{c}, c.Peer(), nil
}
