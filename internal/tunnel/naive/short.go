package naive

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/shorthttps"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/acme/autocert"
)

func shortToken(token string) string {
	s := sha256.Sum256([]byte("backpack managed short HTTPS v1\x00" + token))
	return hex.EncodeToString(s[:])
}

func ValidateShort(c *config.Config) error {
	s, cl := c.Server.Short.Enabled(), c.Client.Short.Enabled()
	if !s && !cl {
		return nil
	}
	if s == cl || c.L3.Enabled() || c.Direct.Enabled() || c.AWG.Enabled() || c.Server.Naive.Enabled() || c.Client.Naive.Enabled() || c.Server.Xray.Enabled() || c.Client.Xray.Enabled() || c.Server.Sing.Enabled() || c.Client.Sing.Enabled() {
		return errors.New("Short HTTPS requires an exclusive TCP Reverse role without another managed wrapper")
	}
	if s {
		v := c.Server
		if v.BindAddr == "" || c.Client.RemoteAddr != "" || v.Transport != config.TCP || v.ForwardsUDP() || len(v.FallbackTransports) != 0 || v.MPTCP {
			return errors.New("Short HTTPS requires TCP Reverse without UDP, MPTCP or transport fallback")
		}
		if _, _, err := hostPort(v.BindAddr, true); err != nil {
			return err
		}
		_, outer, err := hostPort(v.Short.Listen, false)
		if err != nil {
			return err
		}
		_, inner, _ := hostPort(v.BindAddr, true)
		if outer == inner {
			return errors.New("Short HTTPS public and internal ports must differ")
		}
		if v.Token == "" {
			return errors.New("Short HTTPS requires the reverse token")
		}
		x := v.Short
		if x.ACMEDomain != "" {
			if !xrayDNSName(x.ACMEDomain) || net.ParseIP(x.ACMEDomain) != nil || x.ServerName != x.ACMEDomain || x.Certificate != "" || x.Key != "" || outer != 443 {
				return errors.New("Short HTTPS ACME requires the verified public DNS name on port 443, without static PEM files")
			}
			return nil
		}
		if x.ACMEEmail != "" {
			return errors.New("acme_email requires acme_domain")
		}
		_, _, err = validatedTLSMaterial(x.Certificate, x.Key, x.ServerName, "short-https")
		return err
	}
	v := c.Client
	if v.Token == "" {
		return errors.New("Short HTTPS requires the reverse token")
	}
	if v.Short.ServerName != "" && net.ParseIP(v.Short.ServerName) == nil && !xrayDNSName(v.Short.ServerName) {
		return errors.New("Short HTTPS server_name must be a verified hostname or IP")
	}
	if v.RemoteAddr == "" || c.Server.BindAddr != "" || v.Transport != config.TCP || v.Proxy != "" || v.LocalAddr != "" || v.Interface != "" || v.SOMark != 0 || len(v.FallbackAddrs) != 0 || len(v.FallbackTransports) != 0 || v.LoadBalance || v.HealthFailover || v.MPTCP {
		return errors.New("Short HTTPS requires TCP Reverse without another proxy, routing override or fallback")
	}
	if _, _, err := hostPort(v.RemoteAddr, true); err != nil {
		return err
	}
	if _, _, err := xrayEndpoint(v.Short.Server); err != nil {
		return err
	}
	_, err := shortClient(v)
	return err
}

func shortClient(v config.ClientConfig) (*shorthttps.Client, error) {
	var ca []byte
	if v.Short.CAFile != "" {
		if err := readableFile(v.Short.CAFile); err != nil {
			return nil, err
		}
		var err error
		ca, err = os.ReadFile(v.Short.CAFile)
		if err != nil {
			return nil, err
		}
	}
	return shorthttps.NewClientWithServerName("https://"+v.Short.Server, shortToken(v.Token), string(ca), v.Short.ServerName)
}

func StartShortServer(parent context.Context, v *config.ServerConfig, log *logrus.Logger) (*Helper, error) {
	if err := ValidateShort(&config.Config{Server: *v}); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	relay, err := shorthttps.NewServer(ctx, v.BindAddr, shortToken(v.Token))
	if err != nil {
		cancel()
		return nil, err
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}, CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256}}
	if v.Short.ACMEDomain != "" {
		cache := filepath.Join("/etc/backpack/acme", "short-https", v.Short.ACMEDomain)
		if err := os.MkdirAll(cache, 0700); err != nil {
			relay.Close()
			cancel()
			return nil, err
		}
		manager := &autocert.Manager{Prompt: autocert.AcceptTOS, Cache: autocert.DirCache(cache), HostPolicy: autocert.HostWhitelist(v.Short.ACMEDomain), Email: v.Short.ACMEEmail}
		tlsCfg = manager.TLSConfig()
		tlsCfg.MinVersion = tls.VersionTLS13
		tlsCfg.NextProtos = []string{"http/1.1", "acme-tls/1"}
	} else {
		cert, key, err := validatedTLSMaterial(v.Short.Certificate, v.Short.Key, v.Short.ServerName, "short-https")
		if err != nil {
			relay.Close()
			cancel()
			return nil, err
		}
		pair, err := tls.X509KeyPair(cert, key)
		if err != nil {
			relay.Close()
			cancel()
			return nil, err
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
	}
	l, err := tls.Listen("tcp", v.Short.Listen, tlsCfg)
	if err != nil {
		relay.Close()
		cancel()
		return nil, err
	}
	h := &Helper{cancel: cancel, done: make(chan struct{}), addr: l.Addr().String()}
	go func() {
		defer close(h.done)
		defer relay.Close()
		defer reportReadiness(ctx, false)
		for {
			srv := &http.Server{Handler: relay, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 4096}
			stop := context.AfterFunc(ctx, func() { srv.Close(); l.Close() })
			reportReadiness(ctx, true)
			err := srv.Serve(l)
			stop()
			l.Close()
			reportReadiness(ctx, false)
			if ctx.Err() != nil {
				return
			}
			if log != nil {
				log.Warnf("Short HTTPS listener stopped: %v; retrying", err)
			}
			for ctx.Err() == nil {
				if !sleep(ctx, time.Second) {
					return
				}
				l, err = tls.Listen("tcp", v.Short.Listen, tlsCfg)
				if err == nil {
					break
				}
			}
			if ctx.Err() != nil {
				return
			}
		}
	}()
	return h, nil
}

type shortBufferedConn struct {
	*net.TCPConn
	r io.Reader
}

func (c *shortBufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func StartShortClient(parent context.Context, v *config.ClientConfig, log *logrus.Logger) (*Helper, error) {
	if err := ValidateShort(&config.Config{Client: *v}); err != nil {
		return nil, err
	}
	relay, err := shortClient(*v)
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	h := &Helper{cancel: cancel, done: make(chan struct{}), addr: l.Addr().String(), proxyScheme: "http"}
	go func() {
		defer close(h.done)
		defer l.Close()
		defer reportReadiness(ctx, false)
		stop := context.AfterFunc(ctx, func() { l.Close() })
		defer stop()
		var wg sync.WaitGroup
		defer wg.Wait()
		slots := make(chan struct{}, 64)
		reportReadiness(ctx, true)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				conn.Close()
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slots; conn.Close() }()
				stop := context.AfterFunc(ctx, func() { conn.Close() })
				defer stop()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				r := bufio.NewReader(io.LimitReader(conn, 8192))
				req, err := http.ReadRequest(r)
				if err != nil || req.Method != http.MethodConnect || req.Host != v.RemoteAddr || req.ContentLength > 0 || len(req.TransferEncoding) != 0 {
					return
				}
				prefix := make([]byte, r.Buffered())
				if _, err := io.ReadFull(r, prefix); err != nil {
					return
				}
				if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
					return
				}
				conn.SetDeadline(time.Time{})
				stream := &shortBufferedConn{TCPConn: conn.(*net.TCPConn), r: io.MultiReader(bytes.NewReader(prefix), conn)}
				if err := relay.Forward(ctx, stream); err != nil && ctx.Err() == nil && log != nil {
					log.Debugf("Short HTTPS stream ended: %v", err)
				}
			}()
		}
	}()
	return h, nil
}
