package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/backpack/backpack/config"
)

// Certificate and key are generated on Kharej. Only the public trust anchor and
// loopback target port cross the coordinator, authenticated by the test token.
func ctMasqueListener(ctx context.Context, link ConnTestLink, dir, name string, used map[int]bool, carrier ...string) (config.MasqueConfig, error) {
	cert, key, ca, err := ctCertificate(dir, name, "conntest.backpack.invalid")
	if err != nil {
		return config.MasqueConfig{}, err
	}
	port := ctPickPort(used, false)
	if port == 0 {
		return config.MasqueConfig{}, errors.New("no free private MASQUE target port")
	}
	settings := ctCarrierSettings{CA: ca, TargetPort: port, ServerName: "conntest.backpack.invalid"}
	body, _ := json.Marshal(settings)
	raw := gzipB64(body)
	command := "masque-cert"
	if len(carrier) != 0 && carrier[0] == "masque-h2" {
		command = "masque-h2-cert"
	}
	if len(carrier) != 0 && carrier[0] == "webtransport" {
		command = "webtransport-cert"
	}
	reply, err := ctAskContext(ctx, link.Host, link.Coord, command+" "+link.Tok+" "+raw)
	if err != nil || reply != "ok" {
		return config.MasqueConfig{}, fmt.Errorf("could not announce MASQUE trust anchor: %v", err)
	}
	return config.MasqueConfig{CertFile: cert, KeyFile: key, ServerName: settings.ServerName, Target: "127.0.0.1:" + strconv.Itoa(port)}, nil
}
func ctDecodeMasqueSettings(raw string) (ctCarrierSettings, error) {
	var settings ctCarrierSettings
	if len(raw) > 1800 {
		return settings, errors.New("MASQUE identity exceeds test limit")
	}
	body, err := unGzipB64(raw)
	if err != nil || len(body) > 2048 || json.Unmarshal(body, &settings) != nil || settings.TargetPort < 1 || settings.TargetPort > 65535 || settings.ServerName != "conntest.backpack.invalid" {
		return settings, errors.New("invalid MASQUE test identity")
	}
	if _, err = managedLinkCertificates(settings.CA); err != nil || settings.CA == "" {
		return settings, errors.New("invalid public MASQUE certificate")
	}
	return settings, nil
}
func ctMasqueClient(raw, dir, name string) (config.MasqueConfig, error) {
	settings, err := ctDecodeMasqueSettings(raw)
	if err != nil {
		return config.MasqueConfig{}, err
	}
	ca := filepath.Join(dir, name+"-ca.pem")
	if err = os.WriteFile(ca, []byte(settings.CA), 0600); err != nil {
		return config.MasqueConfig{}, err
	}
	return config.MasqueConfig{CAFile: ca, ServerName: settings.ServerName, Target: "127.0.0.1:" + strconv.Itoa(settings.TargetPort)}, nil
}
