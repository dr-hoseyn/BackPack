package naive

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/httpcarrier"
	"github.com/sirupsen/logrus"
)

func ValidateConnect(c *config.Config) error {
	s, cl := c.Server.Connect.Enabled(), c.Client.Connect.Enabled()
	if !s && !cl {
		return nil
	}
	if s == cl || c.AWG.Enabled() || c.Direct.Enabled() || c.L3.Enabled() || c.Server.Naive.Enabled() || c.Client.Naive.Enabled() || c.Server.Xray.Enabled() || c.Client.Xray.Enabled() || c.Server.Sing.Enabled() || c.Client.Sing.Enabled() || c.Server.Short.Enabled() || c.Client.Short.Enabled() || len(c.Server.Paths) > 0 || len(c.Client.Paths) > 0 {
		return errors.New("HTTP CONNECT carriers require one exclusive Reverse role")
	}
	if s {
		v, x := c.Server, c.Server.Connect
		if v.BindAddr == "" || c.Client.RemoteAddr != "" || v.Transport != config.TCP || len(v.FallbackTransports) > 0 || v.MPTCP {
			return errors.New("HTTP CONNECT requires TCP Reverse without transport fallbacks or MPTCP")
		}
		if err := httpcarrier.FixedTarget(v.BindAddr); err != nil {
			return err
		}
		if _, _, err := xrayEndpoint(x.Listen); err != nil {
			return err
		}
		if addrPortOnly(x.Listen) == addrPortOnly(v.BindAddr) {
			return errors.New("public and private HTTP CONNECT ports must differ")
		}
		if err := httpcarrier.Validate(x.Protocol, x.Mode, x.Username, x.Password, x.Path); err != nil {
			return err
		}
		_, _, err := validatedTLSMaterial(x.Certificate, x.Key, x.ServerName, x.Protocol)
		return err
	}
	v, x := c.Client, c.Client.Connect
	if v.RemoteAddr == "" || c.Server.BindAddr != "" || v.Transport != config.TCP || len(v.FallbackTransports) > 0 || len(v.FallbackAddrs) > 0 || v.MPTCP || v.Proxy != "" || v.Interface != "" || v.LocalAddr != "" || v.SOMark != 0 || v.HealthFailover || v.LoadBalance {
		return errors.New("HTTP CONNECT requires TCP Reverse without another proxy, routing overrides or transport fallbacks")
	}
	if err := httpcarrier.FixedTarget(v.RemoteAddr); err != nil {
		return err
	}
	if _, _, err := xrayEndpoint(x.Server); err != nil {
		return err
	}
	if err := httpcarrier.Validate(x.Protocol, x.Mode, x.Username, x.Password, x.Path); err != nil {
		return err
	}
	_, err := connectTLS(x.ServerName, x.CAFile)
	return err
}

func addrPortOnly(a string) string { _, p, _ := net.SplitHostPort(a); return p }
func connectTLS(name, ca string) (*tls.Config, error) {
	if name == "" || (net.ParseIP(name) == nil && !xrayDNSName(name)) {
		return nil, errors.New("HTTP CONNECT needs an explicit verified TLS identity")
	}
	t := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: name}
	if ca != "" {
		b, err := os.ReadFile(ca)
		if err != nil {
			return nil, err
		}
		t.RootCAs = x509.NewCertPool()
		if !t.RootCAs.AppendCertsFromPEM(b) {
			return nil, errors.New("HTTP CONNECT CA file contains no certificates")
		}
	}
	return t, nil
}

func StartConnectServer(parent context.Context, c *config.ServerConfig, log *logrus.Logger) (*Helper, error) {
	if err := ValidateConnect(&config.Config{Server: *c}); err != nil {
		return nil, err
	}
	x := c.Connect
	pair, err := tls.LoadX509KeyPair(x.Certificate, x.Key)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	s, err := httpcarrier.Listen(ctx, x.Protocol, x.Mode, x.Listen, c.BindAddr, x.Username, x.Password, x.Path, &tls.Config{Certificates: []tls.Certificate{pair}})
	if err != nil {
		cancel()
		return nil, err
	}
	h := &Helper{cancel: cancel, done: make(chan struct{}), addr: s.Addr().String()}
	reportReadiness(parent, true)
	go func() {
		defer close(h.done)
		defer reportReadiness(parent, false)
		for {
			select {
			case <-ctx.Done():
				s.Close()
				return
			case <-s.Done():
			}
			s.Close()
			reportReadiness(parent, false)
			for ctx.Err() == nil {
				if !sleep(ctx, time.Second) {
					return
				}
				next, err := httpcarrier.Listen(ctx, x.Protocol, x.Mode, x.Listen, c.BindAddr, x.Username, x.Password, x.Path, &tls.Config{Certificates: []tls.Certificate{pair}})
				if err == nil {
					s = next
					reportReadiness(parent, true)
					break
				}
				if log != nil {
					log.Warnf("HTTP carrier listener restart: %v", err)
				}
			}
			if ctx.Err() != nil {
				return
			}
		}
	}()
	return h, nil
}

func StartConnectClient(parent context.Context, c *config.ClientConfig, log *logrus.Logger) (*Helper, error) {
	if err := ValidateConnect(&config.Config{Client: *c}); err != nil {
		return nil, err
	}
	x := c.Connect
	t, err := connectTLS(x.ServerName, x.CAFile)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	client, err := httpcarrier.NewClient(ctx, x.Protocol, x.Mode, x.Server, x.Username, x.Password, x.Path, t)
	if err != nil {
		cancel()
		return nil, err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		client.Close()
		cancel()
		return nil, err
	}
	h := &Helper{cancel: cancel, done: make(chan struct{}), addr: l.Addr().String()}
	go func() {
		defer close(h.done)
		defer client.Close()
		defer reportReadiness(parent, false)
		stop := context.AfterFunc(ctx, func() { l.Close(); client.Close() })
		defer stop()
		var wg sync.WaitGroup
		defer wg.Wait()
		limit := make(chan struct{}, 256)
		reportReadiness(parent, true)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			select {
			case limit <- struct{}{}:
			default:
				conn.Close()
				continue
			}
			wg.Go(func() {
				defer func() { <-limit }()
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { conn.Close() })
				defer stop()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				if err := fixedConnectSOCKS(conn, c.RemoteAddr); err != nil {
					return
				}
				setup, done := context.WithTimeout(ctx, 5*time.Second)
				stream, err := client.DialStream(setup, c.RemoteAddr)
				done()
				if err != nil {
					conn.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				if _, err = conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
					stream.Close()
					return
				}
				conn.SetDeadline(time.Time{})
				httpcarrier.Relay(ctx, conn, stream)
			})
		}
	}()
	return h, nil
}

// Parse only literal-IP CONNECT requests and compare the canonical destination
// before acknowledging success. No DNS lookup or arbitrary local access occurs.
func fixedConnectSOCKS(c net.Conn, target string) error {
	var head [4]byte
	if _, err := io.ReadFull(c, head[:2]); err != nil {
		return err
	}
	if head[0] != 5 || head[1] == 0 {
		return errors.New("invalid SOCKS greeting")
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return err
	}
	noauth := false
	for _, m := range methods {
		noauth = noauth || m == 0
	}
	if !noauth {
		return errors.New("SOCKS no-auth method required")
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return err
	}
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return err
	}
	if head[0] != 5 || head[1] != 1 || head[2] != 0 {
		return errors.New("SOCKS CONNECT required")
	}
	n := 0
	var encodedIP net.IP
	switch head[3] {
	case 1:
		n = 4
	case 4:
		n = 16
	case 3:
		var length [1]byte
		if _, err := io.ReadFull(c, length[:]); err != nil {
			return err
		}
		name := make([]byte, int(length[0]))
		if _, err := io.ReadFull(c, name); err != nil {
			return err
		}
		// The engine's SOCKS client represents even literal IPs with ATYP=3.
		// Parse the literal directly; never resolve a caller-controlled name.
		encodedIP = net.ParseIP(string(name))
		if encodedIP == nil {
			return errors.New("literal SOCKS destination required")
		}
	default:
		return errors.New("literal SOCKS destination required")
	}
	address := make([]byte, n+2)
	if _, err := io.ReadFull(c, address); err != nil {
		return err
	}
	if encodedIP == nil {
		encodedIP = net.IP(address[:n])
	}
	want, err := net.ResolveTCPAddr("tcp", target)
	if err != nil {
		return err
	}
	if !encodedIP.Equal(want.IP) || int(address[n])<<8|int(address[n+1]) != want.Port {
		return fmt.Errorf("SOCKS destination refused")
	}
	return nil
}
