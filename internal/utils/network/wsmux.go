package network

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const (
	WSMuxOfferHeader   = "X-Backpack-Mux-Versions"
	WSMuxVersionHeader = "X-Backpack-Mux-Version"
	WSMuxPoolHeader    = "X-Backpack-Mux-Pool"
)

// WSMuxOptions belongs to one dial. Missing response headers identify an older
// server: retain the legacy version and do not opt into idle retirement.
type WSMuxOptions struct {
	Version           int
	PoolSize          int
	NegotiatedVersion int
	Retirement        bool
}

func (o *WSMuxOptions) offer(h http.Header) {
	versions := "2,1"
	if o.Version == 1 || o.Version == 2 {
		versions = strconv.Itoa(o.Version)
	}
	h.Set(WSMuxOfferHeader, versions)
	h.Set(WSMuxPoolHeader, strconv.Itoa(o.PoolSize))
}

func (o *WSMuxOptions) accept(h http.Header) error {
	o.NegotiatedVersion = ResolveStaticMuxVersion(o.Version)
	o.Retirement = false
	if h.Get(WSMuxVersionHeader) == "" {
		return nil
	}
	v, err := strconv.Atoi(h.Get(WSMuxVersionHeader))
	if err != nil || (v != 1 && v != 2) || ((o.Version == 1 || o.Version == 2) && v != o.Version) {
		return fmt.Errorf("websocket mux: invalid negotiated version %q", h.Get(WSMuxVersionHeader))
	}
	o.NegotiatedVersion = v
	o.Retirement = h.Get(WSMuxPoolHeader) == strconv.Itoa(o.PoolSize) && o.PoolSize > 0
	return nil
}

// SelectWSMux settles the data connection before either side starts smux. An
// old client sends no offer, retaining the static choice. Incompatible explicit
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
	version = ResolveMuxVersion(configured)
	if !supported[version] {
		if configured == 1 || configured == 2 || !supported[1] {
			return 0, 0, fmt.Errorf("websocket mux: no common protocol version")
		}
		version = 1
	}
	if raw := h.Get(WSMuxPoolHeader); raw != "" {
		floor, err = strconv.Atoi(raw)
		if err != nil || floor < 1 || floor > 65536 {
			return 0, 0, fmt.Errorf("websocket mux: invalid pool floor")
		}
	}
	return version, floor, nil
}
