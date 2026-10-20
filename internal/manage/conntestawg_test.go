package manage

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/backpack/backpack/config"
)

func TestConnTestAWGExchangesOnlyItsPeerIdentity(t *testing.T) {
	if _, err := os.Stat("/bin/true"); err != nil {
		t.Skip("Linux engine fixture")
	}
	old := connTestBinary
	connTestBinary = func() (string, error) { return "/bin/true", nil }
	defer func() { connTestBinary = old }()
	c, err := startCTCoordinator(ctPickPort(map[int]bool{}, true), ctNewSecret())
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	s := &ConnTestIran{root: true, dir: t.TempDir(), coord: c, link: ConnTestLink{Host: "127.0.0.1", Coord: c.tcp.Addr().(*net.TCPAddr).Port, Tok: c.tok, L3: 24001}}
	s.startAWGCase("unit1234", map[int]bool{}, true)
	defer ctStopAll(s.engines)
	if len(s.cases) != 1 || s.cases[0].skip != "" {
		t.Fatalf("AWG case was not prepared: %+v", s.cases)
	}
	link := s.cases[0].link
	peer, err := ctAWGKharej(context.Background(), s.link, link, link.Name)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if _, err := toml.DecodeFile(filepath.Join(s.dir, link.Name+".toml"), &cfg); err != nil {
		t.Fatal(err)
	}
	if peer.PrivateKey == cfg.AWG.PrivateKey || peer.PrivateKey != cfg.AWG.PeerSetupKey || peer.PresharedKey != cfg.AWG.PresharedKey {
		t.Fatal("AWG private identities were not separated and paired")
	}
	if strings.Contains(link.AWGPeer, cfg.AWG.PrivateKey) {
		t.Fatal("Iran private identity escaped into peer data")
	}
	s.link.Cases = []ShareLink{link}
	if len("config "+ctConfig(s.link)) > 1200 || len("carrier "+gzipB64([]byte(link.AWGPeer))) > 1200 {
		t.Fatal("AWG handshake settings exceed one control packet")
	}
	if c.answer("carrier wrong awg", "127.0.0.1") != "" {
		t.Fatal("AWG identity was disclosed without authentication")
	}
}
