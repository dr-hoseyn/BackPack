// Package carrier supplies byte streams to the existing tunnel engines.
// The engine authenticates and encrypts these streams with Noise before
// sending its credential, including when a CDN terminates TLS on the path.
package carrier

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/backpack/backpack/config"
	utls "github.com/refraction-networking/utls"
	"golang.org/x/crypto/ssh"
)

type Config struct {
	Mode    config.TransportType
	Options config.CarrierOptions
	TLS     *tls.Config
}

func (c *Config) DialAddress(addr string) string {
	if c == nil || c.Mode != config.SSH {
		return addr
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	port := c.Options.SSHPort
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func hello(name string) utls.ClientHelloID {
	switch name {
	case "firefox":
		return utls.HelloFirefox_Auto
	case "safari":
		return utls.HelloSafari_Auto
	case "edge":
		return utls.HelloEdge_Auto
	case "random":
		var b [1]byte
		_, _ = rand.Read(b[:])
		return []utls.ClientHelloID{utls.HelloChrome_Auto, utls.HelloFirefox_Auto, utls.HelloSafari_Auto, utls.HelloEdge_Auto}[int(b[0])%4]
	default:
		return utls.HelloChrome_Auto
	}
}

func (c *Config) Client(ctx context.Context, raw net.Conn, addr string, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	raw.SetDeadline(time.Now().Add(timeout))
	defer raw.SetDeadline(time.Time{})
	if c.Mode == config.SSH {
		return c.ssh(raw, addr)
	}
	var wire net.Conn = raw
	if c.Options.TLSFragment {
		wire = &fragmentConn{Conn: raw}
	}
	name := c.Options.ServerName
	if name == "" {
		name, _, _ = net.SplitHostPort(addr)
	}
	// Identity is checked by the Noise handshake inside the carrier, before
	// the tunnel sends a bearer credential. This also permits private origins.
	tc := &utls.Config{ServerName: name, InsecureSkipVerify: true}
	uc := utls.UClient(wire, tc, hello(strings.ToLower(c.Options.Fingerprint)))
	if c.Mode == config.GRPC {
		if err := uc.BuildHandshakeState(); err != nil {
			return nil, err
		}
		for _, ext := range uc.Extensions {
			if a, ok := ext.(*utls.ALPNExtension); ok {
				a.AlpnProtocols = []string{"h2"}
			}
		}
		tc.NextProtos = []string{"h2"}
		if err := uc.BuildHandshakeState(); err != nil {
			return nil, err
		}
	}
	if err := uc.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("%s handshake: %w", c.Mode, err)
	}
	if c.Mode == config.GRPC {
		if uc.ConnectionState().NegotiatedProtocol != "h2" {
			return nil, fmt.Errorf("gRPC peer did not negotiate HTTP/2")
		}
		return grpcClient(ctx, uc, name, c.Options.GRPCService)
	}
	return uc, nil
}

func (c *Config) Server(ctx context.Context, raw net.Conn) (net.Conn, error) {
	if c.Mode == config.SSH {
		return raw, nil
	} // sshd forwards to a loopback-only relay
	raw.SetDeadline(time.Now().Add(15 * time.Second))
	defer raw.SetDeadline(time.Time{})
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	if c.TLS == nil {
		return nil, fmt.Errorf("TLS configuration is missing")
	}
	tc := c.TLS.Clone()
	if c.Mode == config.GRPC {
		tc.NextProtos = []string{"h2"}
	} else {
		tc.NextProtos = []string{"http/1.1"}
	}
	conn := tls.Server(raw, tc)
	if err := conn.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	if c.Mode == config.GRPC {
		if conn.ConnectionState().NegotiatedProtocol != "h2" {
			return nil, fmt.Errorf("gRPC requires HTTP/2")
		}
		return grpcServer(ctx, conn, c.Options.GRPCService)
	}
	return conn, nil
}

type sshConn struct {
	net.Conn
	owner *ssh.Client
	once  sync.Once
}

func (c *sshConn) Close() error {
	var err error
	c.once.Do(func() { c.owner.Close(); err = c.Conn.Close() })
	return err
}
func (c *Config) ssh(raw net.Conn, addr string) (net.Conn, error) {
	o := c.Options
	auth := []ssh.AuthMethod{}
	if o.SSHPassword != "" {
		auth = append(auth, ssh.Password(o.SSHPassword))
	}
	if o.SSHKeyFile != "" {
		b, err := os.ReadFile(o.SSHKeyFile)
		if err != nil {
			return nil, err
		}
		key, err := ssh.ParsePrivateKey(b)
		if err != nil {
			return nil, err
		}
		auth = append(auth, ssh.PublicKeys(key))
	}
	sc := &ssh.ClientConfig{User: o.SSHUser, Auth: auth, HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
		if o.SSHHostKey == "" || ssh.FingerprintSHA256(k) != o.SSHHostKey {
			return fmt.Errorf("SSH host key does not match ssh_host_key")
		}
		return nil
	}}
	cc, ch, rq, err := ssh.NewClientConn(raw, c.DialAddress(addr), sc)
	if err != nil {
		return nil, err
	}
	client := ssh.NewClient(cc, ch, rq)
	target := o.SSHRemote
	if target == "" {
		_, p, _ := net.SplitHostPort(addr)
		target = net.JoinHostPort("127.0.0.1", p)
	}
	chn, err := client.Dial("tcp", target)
	if err != nil {
		client.Close()
		return nil, err
	}
	// Channel deadlines are unsupported by x/crypto/ssh. The owned TCP
	// connection has exactly one channel, so its deadlines apply to that channel.
	return &sshConn{Conn: &deadlineConn{Conn: chn, raw: raw}, owner: client}, nil
}

type deadlineConn struct {
	net.Conn
	raw net.Conn
}

func (c *deadlineConn) SetDeadline(t time.Time) error      { return c.raw.SetDeadline(t) }
func (c *deadlineConn) SetReadDeadline(t time.Time) error  { return c.raw.SetReadDeadline(t) }
func (c *deadlineConn) SetWriteDeadline(t time.Time) error { return c.raw.SetWriteDeadline(t) }

// Split the first write only. Separate writes are a best-effort fragmentation
// hint, not a promise about TCP packet boundaries or a general DPI bypass.
type fragmentConn struct {
	net.Conn
	once sync.Once
}

func (c *fragmentConn) Write(p []byte) (int, error) {
	first := false
	c.once.Do(func() { first = true })
	if !first || len(p) < 64 {
		return c.Conn.Write(p)
	}
	at := len(p) / 2
	n, err := c.Conn.Write(p[:at])
	if err != nil || n != at {
		return n, err
	}
	m, err := c.Conn.Write(p[at:])
	return n + m, err
}
