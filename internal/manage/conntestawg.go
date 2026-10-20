package manage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strconv"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/amneziawg"
)

func (s *ConnTestIran) startAWGCase(id string, used map[int]bool, enabled bool) {
	c := &connTestCase{kind: "awg", tr: "awg", name: "ct-" + id + "-awg"}
	s.cases = append(s.cases, c)
	if !enabled || !s.root {
		c.skip = "AmneziaWG requires root and TUN access on both servers"
		return
	}
	port := ctPickPort(used, true)
	c.entry = ctPickPort(used, true)
	block := ctFreeL3Block(len(ctDirectPlans("")) + 1)
	a, b, err := amneziawg.NewPair(net.JoinHostPort(s.link.Host, strconv.Itoa(port)), block+"1/32", block+"2/32")
	if err != nil {
		c.skip = err.Error()
		return
	}
	a.PresharedKey = ctCaseToken(s.link.Tok, "awg", "awg")
	b.PresharedKey = a.PresharedKey
	a.Interface = "bpawg" + id
	b.Interface = a.Interface
	a.PeerSetupKey = b.PrivateKey
	a.Ports = []string{fmt.Sprintf("127.0.0.1:%d=%s:%d", c.entry, block+"2", s.link.L3)}
	body, err := awgBody(config.AWGConfig(a))
	if err != nil {
		c.skip = err.Error()
		return
	}
	peer, err := json.Marshal(config.AWGConfig(b))
	if err != nil {
		c.skip = err.Error()
		return
	}
	c.link = ShareLink{Kind: "awg", From: "iran", Name: c.name, Tr: "awg", Tok: a.PresharedKey, Port: strconv.Itoa(port), Host: s.link.Host, AWGPeer: string(peer)}
	e, err := startCTEngine(s.dir, c.name, body)
	if err != nil {
		c.skip = err.Error()
		return
	}
	s.engines = append(s.engines, e)
	s.coord.mu.Lock()
	if s.coord.carriers == nil {
		s.coord.carriers = map[string]string{}
	}
	s.coord.carriers["awg"] = gzipB64(peer)
	s.coord.mu.Unlock()
}

func ctAWGKharej(ctx context.Context, link ConnTestLink, c ShareLink, name string) (config.AWGConfig, error) {
	reply, err := ctAskContext(ctx, link.Host, link.Coord, "carrier "+link.Tok+" awg")
	if err != nil || len(reply) < 8 || reply[:8] != "carrier " {
		return config.AWGConfig{}, fmt.Errorf("AmneziaWG peer identity unavailable")
	}
	raw, err := unGzipB64(reply[8:])
	if err != nil || len(raw) > 16<<10 {
		return config.AWGConfig{}, fmt.Errorf("invalid AmneziaWG peer identity")
	}
	c.AWGPeer = string(raw)
	peer, err := awgFromLink(c)
	if err != nil {
		return peer, err
	}
	peer.Interface = "bpawg" + fmt.Sprintf("%x", sha256.Sum256([]byte(name)))[:8]
	return peer, nil
}

// AWG needs a canonical 32-byte key. Existing native test tokens are unchanged.
func ctAWGToken(sum []byte) string { return base64.StdEncoding.EncodeToString(sum[:32]) }
