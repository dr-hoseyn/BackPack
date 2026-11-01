package manage

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

func ctAsk(host string, port int, line string) (string, error) {
	return ctAskContext(context.Background(), host, port, line)
}

// Race UDP after a short TCP head start. A blackholed TCP handshake should
// not add five seconds to every progress poll when UDP still works.
func ctAskContext(parent context.Context, host string, port int, line string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 4*time.Second)
	defer cancel()
	type answer struct {
		value string
		err   error
	}
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return "", fmt.Errorf("a test request requires its secret")
	}
	secret := parts[1]
	answers := make(chan answer, 2)
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	ask := func(network string) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			answers <- answer{err: err}
			return
		}
		defer conn.Close()
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		defer stop()
		if deadline, ok := ctx.Deadline(); ok {
			conn.SetDeadline(deadline)
		}
		packet, nonce, err := ctSeal(secret, "request", nil, []byte(line))
		if err != nil {
			answers <- answer{err: err}
			return
		}
		if network == "tcp" {
			err = ctWritePacket(conn, packet)
		} else {
			_, err = conn.Write(packet)
		}
		if err != nil {
			answers <- answer{err: err}
			return
		}
		var wire []byte
		if network == "tcp" {
			wire, err = ctReadPacket(conn)
		} else {
			b := make([]byte, 65535)
			var n int
			n, err = conn.Read(b)
			wire = b[:n]
		}
		var plain []byte
		if err == nil {
			plain, _, err = ctOpen(secret, "reply", nonce, wire)
		}
		reply := string(plain)
		if err == nil && strings.TrimSpace(reply) == "" {
			err = io.ErrUnexpectedEOF
		}

		answers <- answer{strings.TrimSpace(reply), err}
	}
	go ask("tcp")
	timer := time.NewTimer(150 * time.Millisecond)
	defer timer.Stop()
	launched := false
	completed := 0
	var last error
	for {
		select {
		case a := <-answers:
			completed++
			if a.err == nil {
				return a.value, nil
			}
			last = a.err
			if !launched {
				launched = true
				go ask("udp")
			}
			if completed == 2 {
				return "", fmt.Errorf("test coordinator did not answer TCP or UDP: %w", last)
			}
		case <-timer.C:
			if !launched {
				launched = true
				go ask("udp")
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}
