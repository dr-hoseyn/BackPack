package manage

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestCoordinatorWireAuthenticatesAndBindsReplies(t *testing.T) {
	payload := []byte("setup-start secret permanent-password")
	wire, nonce, err := ctSeal("secret", "request", nil, payload)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte("permanent-password")) {
		t.Fatal("credentials are visible on the wire")
	}
	plain, _, err := ctOpen("secret", "request", nil, wire)
	if err != nil || !bytes.Equal(plain, payload) {
		t.Fatalf("roundtrip: %q %v", plain, err)
	}
	if _, _, err := ctOpen("wrong", "request", nil, wire); err == nil {
		t.Fatal("a different secret authenticated")
	}
	wire[len(wire)-1] ^= 1
	if _, _, err := ctOpen("secret", "request", nil, wire); err == nil {
		t.Fatal("modified ciphertext authenticated")
	}
	reply, _, err := ctSeal("secret", "reply", nonce, []byte("ok"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ctOpen("secret", "reply", nonce, reply); err != nil {
		t.Fatal(err)
	}
	other := append([]byte(nil), nonce...)
	other[0] ^= 1
	if _, _, err := ctOpen("secret", "reply", other, reply); err == nil {
		t.Fatal("reply from another request authenticated")
	}
	if _, _, err := ctOpen("secret", "request", nil, reply); err == nil {
		t.Fatal("a reply authenticated as a request")
	}
}

func TestCoordinatorFrameRejectsUnboundedAllocations(t *testing.T) {
	for _, size := range []uint32{0, 1, uint32(ctWireOverhead - 1), 8193, ^uint32(0)} {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], size)
		if _, err := ctReadPacketLimit(bytes.NewReader(header[:]), 8192); err == nil {
			t.Fatalf("accepted invalid frame size %d", size)
		}
	}
}

func TestCoordinatorEncryptedTCPAndUDP(t *testing.T) {
	port := ctPickPort(map[int]bool{}, true)
	c, err := startCTCoordinator(port, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	for _, network := range []string{"tcp", "udp"} {
		t.Run(network, func(t *testing.T) {
			conn, err := (&net.Dialer{}).DialContext(context.Background(), network, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(time.Second))
			wire, nonce, err := ctSeal("secret", "request", nil, []byte("hello secret"))
			if err != nil {
				t.Fatal(err)
			}
			if network == "tcp" {
				err = ctWritePacket(conn, wire)
			} else {
				_, err = conn.Write(wire)
			}
			if err != nil {
				t.Fatal(err)
			}
			if network == "tcp" {
				wire, err = ctReadPacket(conn)
			} else {
				b := make([]byte, 2048)
				var n int
				n, err = conn.Read(b)
				wire = b[:n]
			}
			if err != nil {
				t.Fatal(err)
			}
			plain, _, err := ctOpen("secret", "reply", nonce, wire)
			if err != nil || string(plain) != "ok" {
				t.Fatalf("reply: %q %v", plain, err)
			}
		})
	}
}

func TestCoordinatorPMTUEncryptionPreservesPacketSize(t *testing.T) {
	c := &ctCoordinator{tok: "secret"}
	for _, size := range []int{ctPMTUMin, 1280, ctPMTUMax} {
		wire, nonce, err := ctPMTUPacket("secret", size, 28)
		if err != nil || len(wire)+28 != size {
			t.Fatalf("IPv4 size %d became %d: %v", size, len(wire)+28, err)
		}
		reply := c.wireAnswer(wire, "127.0.0.1")
		plain, _, err := ctOpen("secret", "reply", nonce, reply)
		if err != nil || string(plain) != "pm "+strconv.Itoa(size) {
			t.Fatalf("PMTU ack: %q %v", plain, err)
		}
	}
	for _, size := range []int{-1, 0, 1, ctPMTUMax + 1} {
		if _, _, err := ctPMTUPacket("secret", size, 28); err == nil {
			t.Fatalf("accepted invalid MTU %d", size)
		}
	}
}
