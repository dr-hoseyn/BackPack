package adaptive

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
)

func fixedSOCKS(conn net.Conn, target string) error {
	var greeting [2]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil {
		return err
	}
	if greeting[0] != 5 || greeting[1] == 0 {
		return errors.New("invalid SOCKS greeting")
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return err
	}
	allowed := false
	for _, m := range methods {
		allowed = allowed || m == 0
	}
	if !allowed {
		return errors.New("SOCKS authentication method refused")
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return err
	}
	var h [4]byte
	if _, err := io.ReadFull(conn, h[:]); err != nil {
		return err
	}
	if h[0] != 5 || h[1] != 1 || h[2] != 0 {
		return errors.New("only fixed CONNECT is supported")
	}
	var host string
	switch h[3] {
	case 1:
		b := make([]byte, 4)
		if _, err := io.ReadFull(conn, b); err != nil {
			return err
		}
		host = net.IP(b).String()
	case 4:
		b := make([]byte, 16)
		if _, err := io.ReadFull(conn, b); err != nil {
			return err
		}
		host = net.IP(b).String()
	case 3:
		var length [1]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return err
		}
		b := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, b); err != nil {
			return err
		}
		ip := net.ParseIP(string(b))
		if ip == nil {
			return errors.New("fixed SOCKS destination must be a literal IP")
		}
		host = ip.String()
	default:
		return errors.New("fixed SOCKS destination must be a literal IP")
	}
	var port [2]byte
	if _, err := io.ReadFull(conn, port[:]); err != nil {
		return err
	}
	if net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))) != target {
		return errors.New("SOCKS target refused")
	}
	return nil
}
