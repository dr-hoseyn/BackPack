package l3

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// One TXT question and one OPT RR. The sealed packet is the data of a private
// EDNS0 option, including its code and length: the entire UDP payload parses
// as DNS, rather than merely starting with a DNS-looking twelve-byte header.
// Noise authenticates packets and rejects replay above this framing layer.
const dnsFrameHeader = 50
const dnsPacketLimit = 16384
const dnsOptionCode = 65001 // RFC 6891 private-use range

func dnsMarker(token string) [8]byte {
	sum := sha256.Sum256([]byte("backpack-dns-label-v1|" + token))
	var marker [8]byte
	hex.Encode(marker[:], sum[:4])
	return marker
}

func putDNSFrame(out []byte, marker [8]byte, sequence uint32, payload []byte) []byte {
	frame := out[:dnsFrameHeader+len(payload)]
	clear(frame[:dnsFrameHeader])
	binary.BigEndian.PutUint16(frame[:2], uint16(sequence))
	frame[2] = 1               // RD=1, query/opcode 0
	frame[5], frame[11] = 1, 1 // QDCOUNT=1, ARCOUNT=1
	frame[12] = 8
	copy(frame[13:21], marker[:])
	frame[21] = 8
	var seq [4]byte
	binary.BigEndian.PutUint32(seq[:], sequence)
	hex.Encode(frame[22:30], seq[:])
	// Root terminator at 30, TXT/IN question at 31..34.
	binary.BigEndian.PutUint16(frame[31:33], 16)
	binary.BigEndian.PutUint16(frame[33:35], 1)
	// Root owner at 35, OPT type at 36, advertised receive size at 38.
	binary.BigEndian.PutUint16(frame[36:38], 41)
	binary.BigEndian.PutUint16(frame[38:40], 4096)
	binary.BigEndian.PutUint16(frame[44:46], uint16(len(payload)+4))
	binary.BigEndian.PutUint16(frame[46:48], dnsOptionCode)
	binary.BigEndian.PutUint16(frame[48:50], uint16(len(payload)))
	copy(frame[dnsFrameHeader:], payload)
	return frame
}

func readDNSFrame(frame []byte, marker [8]byte) ([]byte, bool) {
	if len(frame) < dnsFrameHeader || frame[2] != 1 || frame[3] != 0 || binary.BigEndian.Uint16(frame[4:6]) != 1 || binary.BigEndian.Uint16(frame[6:8]) != 0 || binary.BigEndian.Uint16(frame[8:10]) != 0 || binary.BigEndian.Uint16(frame[10:12]) != 1 {
		return nil, false
	}
	if frame[12] != 8 || string(frame[13:21]) != string(marker[:]) || frame[21] != 8 || frame[30] != 0 || binary.BigEndian.Uint16(frame[31:33]) != 16 || binary.BigEndian.Uint16(frame[33:35]) != 1 {
		return nil, false
	}
	for _, b := range frame[22:30] {
		if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f') {
			return nil, false
		}
	}
	if frame[35] != 0 || binary.BigEndian.Uint16(frame[36:38]) != 41 || binary.BigEndian.Uint32(frame[40:44]) != 0 || binary.BigEndian.Uint16(frame[46:48]) != dnsOptionCode {
		return nil, false
	}
	n := len(frame) - dnsFrameHeader
	if int(binary.BigEndian.Uint16(frame[44:46])) != n+4 || int(binary.BigEndian.Uint16(frame[48:50])) != n {
		return nil, false
	}
	return frame[dnsFrameHeader:], true
}
