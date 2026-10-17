package amneziawg

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

func Run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprintln(out, "Experimental AmneziaWG · Linux · independent point-to-point L3 engine\n  backpack awg init --endpoint <server-IP:port> --dir <new-private-directory>\n  backpack awg check --config <server.json|client.json>\n  backpack awg run --config <server.json|client.json>\nCopy only client.json to the connecting machine. Each file contains its own private key.\nOnly the peer host route is installed. No default route, NAT, forwarding or firewall change.")
		return nil
	}
	f := flag.NewFlagSet("awg "+args[0], flag.ContinueOnError)
	f.SetOutput(out)
	path := f.String("config", "", "private JSON configuration")
	var endpoint, dir, local, peer *string
	if args[0] == "init" {
		endpoint = f.String("endpoint", "", "literal server IP:UDP-port")
		dir = f.String("dir", "", "new private directory")
		local = f.String("address", "10.254.248.1/32", "server tunnel host prefix")
		peer = f.String("peer-address", "10.254.248.2/32", "client tunnel host prefix")
	} else if args[0] != "run" && args[0] != "check" {
		return errors.New("choose init, check or run")
	}
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	if args[0] == "init" {
		return initialize(*endpoint, *dir, *local, *peer, out)
	}
	if *path == "" {
		return errors.New("--config is required")
	}
	cfg, err := Load(*path)
	if err != nil {
		return err
	}
	if args[0] == "check" {
		fmt.Fprintln(out, "AmneziaWG configuration valid; network reachability has not been tested.")
		return nil
	}
	return runDevice(ctx, cfg, out)
}

func initialize(endpoint, dir, address, peer string, out io.Writer) error {
	if dir == "" {
		return errors.New("--dir is required")
	}
	server, client, err := NewPair(endpoint, address, peer)
	if err != nil {
		return err
	}
	// Mkdir is exclusive; never overwrite a pair or remove a pre-existing path.
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			os.Remove(filepath.Join(dir, "server.json"))
			os.Remove(filepath.Join(dir, "client.json"))
			os.Remove(dir)
		}
	}()
	for _, item := range []struct {
		name string
		cfg  Config
	}{{"server.json", server}, {"client.json", client}} {
		body, err := json.MarshalIndent(item.cfg, "", "  ")
		if err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(dir, item.name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := f.Write(append(body, '\n'))
		closeErr := f.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	complete = true
	fmt.Fprintf(out, "Created private AmneziaWG pair in %s.\nCopy only client.json to the connecting machine. Keep server.json local.\nPinned engine: %s · UDP required · experimental\n", dir, Version)
	return nil
}

// NewPair generates independent endpoint identities without opening a socket or writing a file.
func NewPair(endpoint, address, peer string) (Config, Config, error) {
	a, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Config{}, Config{}, err
	}
	b, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Config{}, Config{}, err
	}
	psk, err := freshKey()
	if err != nil {
		return Config{}, Config{}, err
	}
	hpk, err := freshKey()
	if err != nil {
		return Config{}, Config{}, err
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return Config{}, Config{}, err
	}
	headers := [4]string{}
	for i := range headers {
		lo := uint64(binary.LittleEndian.Uint32(entropy[i*4:]))%0x10000000 + uint64(i+1)*0x20000000
		headers[i] = fmt.Sprintf("%d-%d", lo, lo+1023)
	}
	_, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return Config{}, Config{}, errors.New("--endpoint requires a literal server IP:port")
	}
	pn, err := strconv.Atoi(port)
	if err != nil {
		return Config{}, Config{}, errors.New("invalid endpoint port")
	}
	o := Obfuscation{JunkCount: 4, JunkMin: 40, JunkMax: 120, S1: 16, S2: 16, S3: 16, S4: 16,
		H1: headers[0], H2: headers[1], H3: headers[2], H4: headers[3], HeaderProtectionKey: hpk, ContentPadding: 16}
	encode := base64.StdEncoding.EncodeToString
	server := Config{Interface: "bpawg0", Address: address, PeerAddress: peer, ListenPort: pn, PrivateKey: encode(a.Bytes()), PeerPublicKey: encode(b.PublicKey().Bytes()), PresharedKey: psk, MTU: 1280, Obfuscation: o}
	client := server
	client.Address, client.PeerAddress, client.ListenPort, client.Endpoint = peer, address, 0, endpoint
	client.PrivateKey, client.PeerPublicKey, client.Keepalive = encode(b.Bytes()), encode(a.PublicKey().Bytes()), 20
	if err := client.Validate(); err != nil {
		return Config{}, Config{}, err
	}
	if err := server.Validate(); err != nil {
		return Config{}, Config{}, err
	}
	return server, client, nil
}
