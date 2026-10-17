package l3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"sync"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tunnel/naive"
	"github.com/sirupsen/logrus"
)

// Budget QUIC, IPv6, authentication, optional Salamander and proxy framing.
// The helper may fragment a native datagram, but it must never retransmit one.
const singCarrierOverhead = 192

type singCarrier struct {
	net.PacketConn
	helper *naive.Helper
	once   sync.Once
	name   string
}

func (c *singCarrier) Overhead() int       { return singCarrierOverhead }
func (c *singCarrier) CarrierName() string { return c.name }
func (c *singCarrier) Close() error {
	c.once.Do(func() { c.PacketConn.Close(); c.helper.Close() })
	return nil
}

func singCredentials(token, protocol string) (password, uuid, obfs string) {
	sum := sha256.Sum256([]byte("backpack native sing carrier v1\x00" + protocol + "\x00" + token))
	password = hex.EncodeToString(sum[:])
	uuid = fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
	if protocol == "hysteria2" {
		obfs = password
		uuid = ""
	}
	return
}

func validateSingCarrier(cfg Config) error {
	if !config.SingDatagram(cfg.Carrier) {
		return fmt.Errorf("native sing carrier must be hysteria2 or tuic")
	}
	// The route and private UDP destination are checked by the same generator
	// used at runtime, including trust material. No socket or helper is started.
	pw, id, obfs := singCredentials(cfg.Token, cfg.Carrier)
	if cfg.Mode == ModeListen {
		x := config.SingServerConfig{Binary: cfg.Sing.Binary, Protocol: cfg.Carrier, Listen: cfg.Addr, ServerName: cfg.Sing.ServerName, Password: pw, UUID: id, ObfsPassword: obfs, Certificate: cfg.Sing.CertFile, Key: cfg.Sing.KeyFile}
		_, err := naive.SingServerJSON(x, "", cfg.Sing.TargetAddr(), "udp")
		return err
	}
	x := config.SingClientConfig{Binary: cfg.Sing.Binary, Protocol: cfg.Carrier, Server: cfg.Addr, ServerName: cfg.Sing.ServerName, Password: pw, UUID: id, ObfsPassword: obfs, CAFile: cfg.Sing.CAFile}
	_, err := naive.SingClientJSON(x, "127.0.0.1:1080", cfg.Sing.TargetAddr(), "udp")
	return err
}

func openSingCarrier(ctx context.Context, cfg Config) (DatagramCarrier, net.Addr, error) {
	if err := validateSingCarrier(cfg); err != nil {
		return nil, nil, err
	}
	pw, id, obfs := singCredentials(cfg.Token, cfg.Carrier)
	log := logrus.New()
	if cfg.Mode == ModeListen {
		backend, err := net.ListenPacket("udp", cfg.Sing.TargetAddr())
		if err != nil {
			return nil, nil, err
		}
		x := config.SingServerConfig{Binary: cfg.Sing.Binary, Protocol: cfg.Carrier, Listen: cfg.Addr, ServerName: cfg.Sing.ServerName, Password: pw, UUID: id, ObfsPassword: obfs, Certificate: cfg.Sing.CertFile, Key: cfg.Sing.KeyFile}
		helper, err := naive.StartSingListener(ctx, x, cfg.Sing.TargetAddr(), "udp", log)
		if err != nil {
			backend.Close()
			return nil, nil, err
		}
		return &singCarrier{PacketConn: backend, helper: helper, name: cfg.Carrier}, nil, nil
	}
	x := config.SingClientConfig{Binary: cfg.Sing.Binary, Protocol: cfg.Carrier, Server: cfg.Addr, ServerName: cfg.Sing.ServerName, Password: pw, UUID: id, ObfsPassword: obfs, CAFile: cfg.Sing.CAFile}
	helper, err := naive.StartSingProxy(ctx, x, cfg.Sing.TargetAddr(), "udp", log)
	if err != nil {
		return nil, nil, err
	}
	// ProxyURL contains only the private loopback listener, never a credential.
	proxyAddr := helper.ProxyURL()[len("socks5://"):]
	packet, err := naive.NewSOCKSPacketConn(ctx, proxyAddr, cfg.Sing.TargetAddr())
	if err != nil {
		helper.Close()
		return nil, nil, err
	}
	peer, err := net.ResolveUDPAddr("udp", cfg.Sing.TargetAddr())
	if err != nil {
		packet.Close()
		helper.Close()
		return nil, nil, err
	}
	return &singCarrier{PacketConn: packet, helper: helper, name: cfg.Carrier}, peer, nil
}
