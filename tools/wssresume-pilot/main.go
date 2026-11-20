package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	maxStreams         = 2048
	handshakeSize      = 73
	openFlag      byte = 1
	eofFlag       byte = 2
)

type handshake struct {
	id     [16]byte
	nonce  [16]byte
	offset uint64
	flags  byte
}

func (h handshake) write(c *net.TCPConn, key []byte, mode byte) error {
	var packet [handshakeSize]byte
	copy(packet[:16], h.id[:])
	copy(packet[16:32], h.nonce[:])
	binary.BigEndian.PutUint64(packet[32:40], h.offset)
	packet[40] = h.flags
	tag := mac(key, mode, packet[:41])
	copy(packet[41:], tag)
	_, err := c.Write(packet[:])
	return err
}

func mac(key []byte, mode byte, b []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte{'B', 'P', 'R', '1', mode})
	h.Write(b)
	return h.Sum(nil)
}

func readHandshake(c *net.TCPConn, key []byte, mode byte) (handshake, error) {
	var h handshake
	var packet [handshakeSize]byte
	if _, err := io.ReadFull(c, packet[:]); err != nil {
		return h, err
	}
	if !hmac.Equal(packet[41:], mac(key, mode, packet[:41])) {
		return h, errProtocol
	}
	copy(h.id[:], packet[:16])
	copy(h.nonce[:], packet[16:32])
	h.offset = binary.BigEndian.Uint64(packet[32:40])
	h.flags = packet[40]
	if h.flags & ^(openFlag|eofFlag) != 0 {
		return h, errProtocol
	}
	return h, nil
}

func connect(addr *net.TCPAddr, timeout time.Duration) (*net.TCPConn, error) {
	conn, err := net.DialTimeout("tcp", addr.String(), timeout)
	if err != nil {
		return nil, err
	}
	return conn.(*net.TCPConn), nil
}

func eofFlags(eof bool) byte {
	if eof {
		return eofFlag
	}
	return 0
}

func edge(app *net.TCPConn, target *net.TCPAddr, key []byte, trace bool) {
	s := newStream(app)
	defer s.close()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return
	}

	first := true
	retryUntil := time.Now().Add(5 * time.Second)
	for time.Now().Before(retryUntil) {
		select {
		case <-s.done:
			return
		default:
		}
		link, err := connect(target, min(750*time.Millisecond, time.Until(retryUntil)))
		if err == nil {
			localOffset, localEOF := s.receiveState()
			req := handshake{id: id, offset: localOffset, flags: eofFlags(localEOF)}
			_, err = rand.Read(req.nonce[:])
			if first {
				req.flags |= openFlag
			}
			_ = link.SetDeadline(minTime(time.Now().Add(2*time.Second), retryUntil))
			if err == nil {
				err = req.write(link, key, 'Q')
			}
			var reply handshake
			if err == nil {
				reply, err = readHandshake(link, key, 'R')
			}
			if err == nil && (reply.id != id || reply.nonce != req.nonce || reply.flags&openFlag != 0) {
				err = errProtocol
			}
			if err == nil {
				err = s.acknowledge(reply.offset, reply.flags&eofFlag != 0)
			}
			if err == nil {
				_ = link.SetDeadline(time.Time{})
				if trace {
					log.Printf("LINK user_port=%d source_port=%d resumed=%t", app.RemoteAddr().(*net.TCPAddr).Port, link.LocalAddr().(*net.TCPAddr).Port, !first)
				}
				if first {
					go s.collect()
					first = false
				}
				s.transfer(&carrier{conn: link, done: make(chan struct{})}, reply.offset, reply.flags&eofFlag != 0, localOffset, localEOF)
				retryUntil = time.Now().Add(resumeTimeout)
				continue
			}
			_ = link.Close()
			if first || err == errProtocol {
				return
			}
		}
		select {
		case <-s.done:
			return
		case <-time.After(75 * time.Millisecond):
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

type relay struct {
	key      []byte
	target   *net.TCPAddr
	mu       sync.Mutex
	sessions map[[16]byte]*stream
}

func (r *relay) session(req handshake) (*stream, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.flags&openFlag == 0 {
		if s := r.sessions[req.id]; s != nil {
			return s, nil
		}
		return nil, errProtocol
	}
	if req.offset != 0 || req.flags&eofFlag != 0 || r.sessions[req.id] != nil || len(r.sessions) >= maxStreams {
		return nil, errProtocol
	}
	app, err := connect(r.target, 500*time.Millisecond)
	if err != nil {
		return nil, err
	}
	s := newStream(app)
	r.sessions[req.id] = s
	go s.collect()
	go func() {
		<-s.done
		r.mu.Lock()
		delete(r.sessions, req.id)
		r.mu.Unlock()
	}()
	return s, nil
}

func (r *relay) accept(c *net.TCPConn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	req, err := readHandshake(c, r.key, 'Q')
	if err != nil {
		return
	}
	s, err := r.session(req)
	if err != nil {
		return
	}
	link := &carrier{conn: c, done: make(chan struct{})}
	s.attach.Lock()
	if s.active != nil {
		_ = s.active.conn.Close()
		<-s.active.done
	}
	select {
	case <-s.done:
		s.attach.Unlock()
		return
	default:
	}
	if err := s.acknowledge(req.offset, req.flags&eofFlag != 0); err != nil {
		s.attach.Unlock()
		s.close()
		return
	}
	s.active = link
	localOffset, localEOF := s.receiveState()
	reply := handshake{id: req.id, nonce: req.nonce, offset: localOffset, flags: eofFlags(localEOF)}
	err = reply.write(c, r.key, 'R')
	s.attach.Unlock()
	if err == nil {
		_ = c.SetDeadline(time.Time{})
		s.transfer(link, req.offset, req.flags&eofFlag != 0, localOffset, localEOF)
	} else {
		close(link.done)
	}
	time.AfterFunc(resumeTimeout, func() {
		s.attach.Lock()
		defer s.attach.Unlock()
		if s.active == link {
			s.close()
		}
	})
}

func loopback(raw string) (*net.TCPAddr, error) {
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("pilot endpoints require a literal loopback address")
	}
	return net.ResolveTCPAddr("tcp", net.JoinHostPort(ip.String(), port))
}

func run(mode, bind, next, keyFile string, trace bool) error {
	if mode != "edge" && mode != "exit" {
		return errors.New("mode must be edge or exit")
	}
	local, err := loopback(bind)
	if err != nil {
		return err
	}
	target, err := loopback(next)
	if err != nil {
		return err
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		return err
	}
	key = []byte(strings.TrimSpace(string(key)))
	if len(key) < 32 {
		return errors.New("pilot key must have at least 32 bytes")
	}
	ln, err := net.ListenTCP("tcp", local)
	if err != nil {
		return err
	}
	defer ln.Close()
	r := &relay{key: key, target: target, sessions: make(map[[16]byte]*stream)}
	inFlight := make(chan struct{}, maxStreams)
	// Both roles listen on loopback. Only the authenticated tunnel reaches
	// the exit, and the configured backend is fixed for this listener.
	for {
		c, err := ln.AcceptTCP()
		if err != nil {
			return err
		}
		select {
		case inFlight <- struct{}{}:
			go func() {
				defer func() { <-inFlight }()
				if mode == "edge" {
					edge(c, target, key, trace)
				} else {
					r.accept(c)
				}
			}()
		default:
			_ = c.Close()
		}
	}
}

func main() {
	mode := flag.String("mode", "", "edge or exit")
	bind := flag.String("listen", "", "loopback listen address")
	next := flag.String("target", "", "loopback target address")
	key := flag.String("key-file", "", "shared pilot authentication key file")
	trace := flag.Bool("trace", false, "report mapped ephemeral ports")
	flag.Parse()
	if err := run(*mode, *bind, *next, *key, *trace); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
