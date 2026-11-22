package network

import (
	"fmt"
	"io"

	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"
)

const (
	WSMuxOfferHeader     = "X-Backpack-Mux-Versions"
	WSMuxVersionHeader   = "X-Backpack-Mux-Version"
	WSMuxPoolHeader      = "X-Backpack-Mux-Pool"
	WSMuxIdleHeader      = "X-Backpack-Mux-Idle"
	WSMuxAdmissionHeader = "X-Backpack-Mux-Admission"
	WSMuxCapacityHeader  = "X-Backpack-Mux-Capacity"
)

// WSMuxOptions belongs to one dial. Missing response headers identify an older
// server: retain the legacy version and do not opt into idle retirement.
type WSMuxOptions struct {
	Version           int
	PoolSize          int
	NegotiatedVersion int
	Retirement        bool
	ConfirmAdmission  bool
	LocalIdle         bool // request per-carrier retirement; cleared unless acknowledged
}

func (o *WSMuxOptions) offer(h http.Header) {
	versions := "1,2"
	if o.Version == 1 || o.Version == 2 {
		versions = strconv.Itoa(o.Version)
	}
	h.Set(WSMuxOfferHeader, versions)
	h.Set(WSMuxPoolHeader, strconv.Itoa(o.PoolSize))
	if o.ConfirmAdmission {
		h.Set(WSMuxAdmissionHeader, "1")
	}
	if o.LocalIdle {
		h.Set(WSMuxIdleHeader, "1")
	}
}

func (o *WSMuxOptions) accept(h http.Header) error {
	o.NegotiatedVersion = ResolveStaticMuxVersion(o.Version)
	o.Retirement = false
	if h.Get(WSMuxVersionHeader) == "" {
		o.LocalIdle = false
		return nil
	}
	v, err := strconv.Atoi(h.Get(WSMuxVersionHeader))
	if err != nil || (v != 1 && v != 2) || ((o.Version == 1 || o.Version == 2) && v != o.Version) {
		return fmt.Errorf("websocket mux: invalid negotiated version %q", h.Get(WSMuxVersionHeader))
	}
	o.NegotiatedVersion = v
	o.Retirement = h.Get(WSMuxPoolHeader) == strconv.Itoa(o.PoolSize) && o.PoolSize > 0
	o.LocalIdle = o.LocalIdle && o.Retirement && h.Get(WSMuxIdleHeader) == "1"
	return nil
}

// SelectWSMux settles the data connection before either side starts smux. An
// old client sends no offer, retaining the static choice. Auto also retains
// version 1: changing flow control by default degraded lossy-path tail latency.
// An explicit version 2 on either new peer negotiates 2. Incompatible explicit
// choices fail during setup instead of silently carrying no traffic.
func SelectWSMux(h http.Header, configured int) (version, floor int, err error) {
	if h.Get(WSMuxOfferHeader) == "" {
		return ResolveStaticMuxVersion(configured), 0, nil
	}
	var supported [3]bool
	for _, part := range strings.Split(h.Get(WSMuxOfferHeader), ",") {
		switch strings.TrimSpace(part) {
		case "1":
			supported[1] = true
		case "2":
			supported[2] = true
		default:
			return 0, 0, fmt.Errorf("websocket mux: invalid version offer")
		}
	}
	version = ResolveStaticMuxVersion(configured)
	if !supported[version] {
		if configured == 1 || configured == 2 || !supported[2] {
			return 0, 0, fmt.Errorf("websocket mux: no common protocol version")
		}
		version = 2
	}
	if raw := h.Get(WSMuxPoolHeader); raw != "" {
		floor, err = strconv.Atoi(raw)
		if err != nil || floor < 1 || floor > 65536 {
			return 0, 0, fmt.Errorf("websocket mux: invalid pool floor")
		}
	}
	return version, floor, nil
}

const wsMuxAdmitted = 1

func readWSMuxAdmission(conn *websocket.Conn) error {
	kind, reader, err := conn.NextReader()
	if err != nil {
		return fmt.Errorf("websocket mux admission: %w", err)
	}
	var frame [2]byte
	n, err := io.ReadFull(reader, frame[:])
	if kind != websocket.BinaryMessage || n != 1 || err != io.ErrUnexpectedEOF || frame[0] != wsMuxAdmitted {
		return fmt.Errorf("websocket mux: invalid admission confirmation")
	}
	return nil
}
