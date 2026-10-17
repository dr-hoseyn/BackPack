package manage

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/backpack/backpack/config"
)

func ctSingListener(ctx context.Context, link ConnTestLink, protocol, dir, name string, used map[int]bool) (config.SingL3Config, error) {
	bin, err := connTestHelperBinary("sing-box")
	if err != nil {
		return config.SingL3Config{}, err
	}
	cert, key, ca, err := ctCertificate(dir, name, "conntest.backpack.invalid")
	if err != nil {
		return config.SingL3Config{}, err
	}
	port := ctPickPort(used, false)
	if port == 0 {
		return config.SingL3Config{}, fmt.Errorf("no private datagram target port")
	}
	settings := ctCarrierSettings{CA: ca, TargetPort: port, ServerName: "conntest.backpack.invalid"}
	body, _ := json.Marshal(settings)
	reply, err := ctAskContext(ctx, link.Host, link.Coord, "sing-cert "+link.Tok+" "+protocol+":"+gzipB64(body))
	if err != nil || reply != "ok" {
		return config.SingL3Config{}, fmt.Errorf("could not announce native %s test identity", protocol)
	}
	return config.SingL3Config{Binary: bin, CertFile: cert, KeyFile: key, ServerName: settings.ServerName, Target: "127.0.0.1:" + strconv.Itoa(port)}, nil
}

func ctSingClient(raw, dir, name string) (config.SingL3Config, error) {
	bin, err := connTestHelperBinary("sing-box")
	if err != nil {
		return config.SingL3Config{}, err
	}
	m, err := ctMasqueClient(raw, dir, name)
	if err != nil {
		return config.SingL3Config{}, err
	}
	return config.SingL3Config{Binary: bin, Target: m.Target, ServerName: m.ServerName, CAFile: m.CAFile}, nil
}

func ctAcceptSingIdentity(c *ctCoordinator, value string) string {
	protocol, raw, ok := strings.Cut(value, ":")
	if !ok || !config.SingDatagram(protocol) {
		return ""
	}
	if _, err := ctDecodeMasqueSettings(raw); err != nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := "direct/" + protocol
	if c.peer != "" || c.carriers[key] != "" && c.carriers[key] != raw {
		return ""
	}
	if c.carriers == nil {
		c.carriers = make(map[string]string)
	}
	c.carriers[key] = raw
	return "ok"
}
