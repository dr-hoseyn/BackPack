package naive

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/BurntSushi/toml"
	"github.com/sirupsen/logrus"
	"io"
	"net"
	"os"
	"runtime"
)

// RealmServerConfig is a dedicated rendezvous service with a bounded account
// registration quota. It configures no NAT, firewall rules or outbound proxy.
type RealmServerConfig struct {
	Binary      string `toml:"binary"`
	Listen      string `toml:"listen"`
	ServerName  string `toml:"server_name"`
	Certificate string `toml:"certificate"`
	Key         string `toml:"key"`
	Token       string `toml:"token"`
	MaxRealms   int    `toml:"max_realms"`
}

func RealmServerJSON(x RealmServerConfig, ready string) ([]byte, error) {
	if err := credentials(x.Binary, "backpack", x.Token); err != nil {
		return nil, err
	}
	h, p, err := xrayEndpoint(x.Listen)
	if err != nil {
		return nil, err
	}
	if x.MaxRealms < 1 || x.MaxRealms > 128 {
		return nil, errors.New("max_realms must be 1..128")
	}
	cert, key, err := validatedTLSMaterial(x.Certificate, x.Key, x.ServerName, "hysteria-realm")
	if err != nil {
		return nil, err
	}
	rh, rp, err := hostPort(ready, true)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"log":      map[string]any{"level": "warn"},
		"services": []any{map[string]any{"type": "hysteria-realm", "listen": h, "listen_port": p, "users": []any{map[string]any{"name": "backpack", "token": x.Token, "max_realms": x.MaxRealms}}, "tls": map[string]any{"enabled": true, "certificate": []string{string(cert)}, "key": []string{string(key)}}}},
		"inbounds": []any{map[string]any{"type": "socks", "listen": rh, "listen_port": rp}},
		"route":    map[string]any{"rules": []any{map[string]any{"action": "reject"}}},
	})
}

// RunRealm implements `backpack realm check|run -c FILE`; its caller owns signals.
func RunRealm(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "check" && args[0] != "run") {
		return errors.New("usage: backpack realm check|run -c realm.toml")
	}
	flags := flag.NewFlagSet("realm", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("c", "", "private Realm TOML configuration")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *path == "" || flags.NArg() != 0 {
		return errors.New("provide -c realm.toml")
	}
	f, err := os.Open(*path)
	if err != nil {
		return errors.New("cannot open Realm configuration")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
		return errors.New("Realm configuration must be a regular file of at most 64 KiB")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return errors.New("Realm configuration contains a token; set mode 0600")
	}
	var cfg struct {
		Realm RealmServerConfig `toml:"realm"`
	}
	metadata, err := toml.NewDecoder(io.LimitReader(f, 65536)).Decode(&cfg)
	if err != nil || len(metadata.Undecoded()) != 0 {
		return errors.New("invalid Realm TOML configuration or unknown key")
	}
	if _, err := RealmServerJSON(cfg.Realm, "127.0.0.1:1"); err != nil {
		return err
	}
	if args[0] == "check" {
		fmt.Fprintln(out, "Realm configuration validated; remote registration and NAT traversal were not measured.")
		return nil
	}
	if runtime.GOOS != "linux" {
		return errors.New("running a Realm service requires Linux")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer l.Close()
	public, err := net.Listen("tcp", cfg.Realm.Listen)
	if err != nil {
		return err
	}
	defer public.Close()
	body, err := RealmServerJSON(cfg.Realm, l.Addr().String())
	if err != nil {
		return err
	}
	h, err := startManaged(ctx, cfg.Realm.Binary, "sing-realm", body, l.Addr().String(), os.Environ(), logrus.StandardLogger(), func() error { public.Close(); return l.Close() })
	if err != nil {
		return err
	}
	defer h.Close()
	fmt.Fprintln(out, "Realm service started; only configured account registrations are accepted.")
	<-ctx.Done()
	return nil
}
