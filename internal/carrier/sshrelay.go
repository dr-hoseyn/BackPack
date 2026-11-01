package carrier

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
)

// StartTestSSH is a temporary, forwarding-only SSH endpoint for Connection
// Test. It cannot run commands and can reach only the test's loopback relay.
// Production SSH carriers use the operator's existing sshd instead.
func StartTestSSH(ctx context.Context, listener net.Listener, target, password string) (string, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return "", err
	}
	cfg := &ssh.ServerConfig{MaxAuthTries: 2, PasswordCallback: func(meta ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		if meta.User() != "backpack-test" || subtle.ConstantTimeCompare(p, []byte(password)) != 1 {
			return nil, fmt.Errorf("authentication failed")
		}
		return nil, nil
	}}
	cfg.AddHostKey(signer)
	slots := make(chan struct{}, 128)
	go func() {
		stop := context.AfterFunc(ctx, func() { listener.Close() })
		defer stop()
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				raw.Close()
				continue
			}
			go func() {
				defer func() { <-slots }()
				defer raw.Close()
				stop := context.AfterFunc(ctx, func() { raw.Close() })
				defer stop()
				raw.SetDeadline(time.Now().Add(10 * time.Second))
				sc, chans, reqs, err := ssh.NewServerConn(raw, cfg)
				raw.SetDeadline(time.Time{})
				if err != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					var request struct {
						Host       string
						Port       uint32
						Origin     string
						OriginPort uint32
					}
					if ch.ChannelType() != "direct-tcpip" || ssh.Unmarshal(ch.ExtraData(), &request) != nil || net.JoinHostPort(request.Host, strconv.Itoa(int(request.Port))) != target {
						ch.Reject(ssh.Prohibited, "test relay only")
						continue
					}
					backend, err := (&net.Dialer{}).DialContext(ctx, "tcp", target)
					if err != nil {
						ch.Reject(ssh.ConnectionFailed, "relay unavailable")
						continue
					}
					channel, requests, err := ch.Accept()
					if err != nil {
						backend.Close()
						continue
					}
					go ssh.DiscardRequests(requests)
					go func() {
						defer backend.Close()
						defer channel.Close()
						done := make(chan struct{})
						go func() { io.Copy(backend, channel); backend.Close(); close(done) }()
						io.Copy(channel, backend)
						channel.Close()
						<-done
					}()
				}
			}()
		}
	}()
	return ssh.FingerprintSHA256(signer.PublicKey()), nil
}
