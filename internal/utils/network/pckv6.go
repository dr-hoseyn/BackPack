package network

import (
	"encoding/binary"
	"golang.org/x/net/bpf"
	"net"
)

func parsePckV6Frame(frame []byte, linkLen int, port uint16) (pckSegment, bool) {
	var s pckSegment
	if linkLen < 0 || len(frame) < linkLen+40 {
		return s, false
	}
	if linkLen == 14 && binary.BigEndian.Uint16(frame[12:14]) != 0x86dd {
		return s, false
	}
	ip := frame[linkLen:]
	if ip[0]>>4 != 6 || ip[6] != 6 {
		return s, false
	}
	end := 40 + int(binary.BigEndian.Uint16(ip[4:6]))
	if end > len(ip) || end < 60 {
		return s, false
	}
	tcp := ip[40:end]
	off := int(tcp[12]>>4) * 4
	if off < 20 || off > len(tcp) || binary.BigEndian.Uint16(tcp[2:4]) != port {
		return s, false
	}
	s.SrcIP = append(net.IP(nil), ip[8:24]...)
	s.SrcPort = binary.BigEndian.Uint16(tcp[:2])
	s.DstPort = port
	s.Seq = binary.BigEndian.Uint32(tcp[4:8])
	s.TSVal = tcpTimestamp(tcp[20:off])
	s.Payload = tcp[off:]
	return s, true
}
func pckV6BPFProgram(port uint16, linkLen int) ([]bpf.RawInstruction, error) {
	l := uint32(linkLen)
	return bpf.Assemble([]bpf.Instruction{
		bpf.LoadAbsolute{Off: l, Size: 1}, bpf.ALUOpConstant{Op: bpf.ALUOpAnd, Val: 0xf0}, bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: 0x60, SkipTrue: 5},
		bpf.LoadAbsolute{Off: l + 6, Size: 1}, bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: 6, SkipTrue: 3},
		bpf.LoadAbsolute{Off: l + 42, Size: 2}, bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: uint32(port), SkipTrue: 1},
		bpf.RetConstant{Val: 0xffff}, bpf.RetConstant{Val: 0},
	})
}
