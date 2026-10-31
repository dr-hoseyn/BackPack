package l3

import (
	"bytes"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestDNSFramesAreCompleteQueriesWithAnEDNSOption(t *testing.T) {
	payload := bytes.Repeat([]byte{0x71, 0x04, 0xff}, 400)
	marker := dnsMarker("a private test token")
	frame := putDNSFrame(make([]byte, 4096), marker, 123, payload)
	var msg dnsmessage.Message
	if err := msg.Unpack(frame); err != nil {
		t.Fatalf("the wire is not valid DNS: %v", err)
	}
	if msg.Response || len(msg.Questions) != 1 || msg.Questions[0].Type != dnsmessage.TypeTXT || len(msg.Additionals) != 1 {
		t.Fatalf("wrong DNS query shape: %+v", msg)
	}
	opt, ok := msg.Additionals[0].Body.(*dnsmessage.OPTResource)
	if !ok || len(opt.Options) != 1 || opt.Options[0].Code != dnsOptionCode || !bytes.Equal(opt.Options[0].Data, payload) {
		t.Fatal("DNS parser did not recover the complete EDNS payload")
	}
	got, ok := readDNSFrame(frame, marker)
	if !ok || !bytes.Equal(got, payload) {
		t.Fatal("DNS wrapping changed a sealed packet")
	}
	if _, ok = readDNSFrame(frame, dnsMarker("another tunnel")); ok {
		t.Fatal("a different tunnel's DNS frame passed the cheap demultiplexer")
	}
}

func TestTruncatedOrMalformedDNSCannotReachTheTunnel(t *testing.T) {
	marker := dnsMarker("framing test")
	frame := putDNSFrame(make([]byte, 512), marker, 42, []byte("sealed noise packet"))
	for n := 0; n < len(frame); n++ {
		if _, ok := readDNSFrame(frame[:n], marker); ok {
			t.Fatalf("accepted a DNS query cut at %d", n)
		}
	}
	for _, offset := range []int{2, 3, 4, 5, 7, 9, 11, 12, 21, 30, 32, 34, 35, 37, 40, 44, 45, 46, 47, 48, 49} {
		bad := append([]byte(nil), frame...)
		bad[offset] ^= 0x80
		if _, ok := readDNSFrame(bad, marker); ok {
			t.Fatalf("accepted a malformed DNS field at %d", offset)
		}
	}
}
