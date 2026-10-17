package network

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"

	"golang.org/x/net/bpf"
)

func TestIPv6PckFramingFilterAndChecksum(t *testing.T) {
	src, dst := net.ParseIP("2001:db8::1"), net.ParseIP("2001:db8::2")
	tcp := buildPckTCP(12345, 9000, 42, 43, FlagACK, 11, 12, src, dst, []byte("test-ipv6"))
	// Verify against a separately assembled RFC 8200 pseudo-header and checksum
	// accumulator, including the on-wire checksum rather than clearing it.
	pseudo := append(append([]byte{}, src.To16()...), dst.To16()...)
	pseudo = append(pseudo, 0, 0, 0, byte(len(tcp)), 0, 0, 0, 6)
	wire := append(pseudo, tcp...)
	var sum uint32
	for i := 0; i < len(wire); i += 2 {
		sum += uint32(wire[i]) << 8
		if i+1 < len(wire) {
			sum += uint32(wire[i+1])
		}
	}
	for sum > 65535 {
		sum = sum>>16 + sum&65535
	}
	if sum != 65535 {
		t.Fatalf("invalid IPv6 TCP checksum: %x", sum)
	}
	ip := make([]byte, 40)
	ip[0] = 0x60
	ip[6] = 6
	ip[7] = 64
	binary.BigEndian.PutUint16(ip[4:6], uint16(len(tcp)))
	copy(ip[8:24], src.To16())
	copy(ip[24:40], dst.To16())
	ip = append(ip, tcp...)
	for _, link := range []int{0, 14} {
		frame := append(make([]byte, link), ip...)
		if link == 14 {
			binary.BigEndian.PutUint16(frame[12:14], 0x86dd)
		}
		seg, ok := parsePckFrame(frame, link, 9000)
		if !ok || !seg.SrcIP.Equal(src) || seg.SrcPort != 12345 || !bytes.Equal(seg.Payload, []byte("test-ipv6")) {
			t.Fatalf("parse link=%d: %+v %v", link, seg, ok)
		}
		raw, err := pckV6BPFProgram(9000, link)
		if err != nil {
			t.Fatal(err)
		}
		prog, _ := bpf.Disassemble(raw)
		vm, err := bpf.NewVM(prog)
		if err != nil {
			t.Fatal(err)
		}
		n, err := vm.Run(frame)
		if err != nil || n == 0 {
			t.Fatalf("valid packet rejected: %d %v", n, err)
		}
		for _, bad := range []int{6, 42} {
			mutated := append([]byte(nil), frame...)
			mutated[link+bad] ^= 1
			if n, _ := vm.Run(mutated); n != 0 {
				t.Fatalf("bad next-header/port accepted: %d", bad)
			}
		}
		for end := 0; end < len(frame); end++ {
			if _, ok := parsePckFrame(frame[:end], link, 9000); ok {
				t.Fatalf("truncation %d accepted", end)
			}
		}
	}
}
