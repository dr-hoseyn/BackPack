package manage

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tui"
	"github.com/backpack/backpack/internal/tunnel/naive"
)

func configureSing(n *TunnelSpec, old TunnelSpec, protocol, public, host string) bool {
	client := old.SingClient
	server := old.SingServer
	if selectedTransport(old) != protocol {
		client, server = config.SingClientConfig{}, config.SingServerConfig{}
	}
	if n.Role == "client" {
		client.Protocol, client.Server = protocol, public
		client.Binary = tui.PromptDefault("Sing-Box Binary", defaultString(client.Binary, managedHelperPath("sing-box")))
		client.ServerName = tui.PromptDefault("Verified TLS Name (From Iran)", defaultString(client.ServerName, host))
		client.Password = managedSecret("Helper Password (From Iran)", client.Password, "")
		if protocol == "tuic" {
			client.UUID = tui.PromptDefault("TUIC UUID (From Iran)", client.UUID)
		}
		if protocol == "hysteria2" {
			client.ObfsPassword = managedSecret("Obfuscation Password (Empty = Disabled)", client.ObfsPassword, "")
			if client.ObfsPassword != "" {
				client.ObfsType = tui.PromptDefault("Obfuscation Type (salamander/gecko)", defaultString(client.ObfsType, "salamander"))
			} else {
				client.ObfsType = ""
			}
			if client.ObfsType != "gecko" {
				client.GeckoMin, client.GeckoMax = 0, 0
			}
			client.BBRProfile = tui.PromptDefault("BBR Profile (standard/conservative/aggressive)", defaultString(client.BBRProfile, "standard"))
			client.Realm = configureRealm(client.Realm)
		}
		if protocol == "shadowtls" {
			client.ShadowsocksKey = managedSecret("SS2022 Base64 Key (From Iran)", client.ShadowsocksKey, "")
		}
		client.CAFile = tui.PromptDefault("Private CA File (Empty = System Trust)", client.CAFile)
		if config.SingDatagram(protocol) {
			client.NativeUDP = tui.Confirm("Use Native Reverse UDP (Must Match Iran)", client.NativeUDP)
		}
		if !client.NativeUDP {
			client.UDPMaxFlows, client.UDPBandwidthMbps = 0, 0
		}
		n.SingClient = client
		return true
	}
	server.Protocol, server.Listen = protocol, public
	server.Binary = tui.PromptDefault("Sing-Box Binary", defaultString(server.Binary, managedHelperPath("sing-box")))
	if protocol == "shadowtls" {
		server.Handshake = strings.TrimSpace(tui.PromptDefault("ShadowTLS Cover Endpoint (Hostname:Port)", server.Handshake))
		host = addrHost(server.Handshake, "")
	}
	server.ServerName = tui.PromptDefault("Verified TLS Name", defaultString(server.ServerName, host))
	server.Password = managedSecret("Helper Password", server.Password, randomToken(32))
	if protocol == "tuic" {
		id := server.UUID
		if id == "" {
			var err error
			id, err = managedUUID()
			if err != nil {
				tui.Error(err.Error())
				return false
			}
		}
		server.UUID = tui.PromptDefault("TUIC UUID", id)
	}
	if protocol == "hysteria2" {
		if tui.Confirm("Enable Obfuscation (Both Sides Must Match)", server.ObfsPassword != "") {
			server.ObfsPassword = managedSecret("Obfuscation Password", server.ObfsPassword, randomToken(32))
			server.ObfsType = tui.PromptDefault("Obfuscation Type (salamander/gecko)", defaultString(server.ObfsType, "salamander"))
		} else {
			server.ObfsPassword = ""
			server.ObfsType, server.GeckoMin, server.GeckoMax = "", 0, 0
		}
		if server.ObfsType != "gecko" {
			server.GeckoMin, server.GeckoMax = 0, 0
		}
		server.BBRProfile = tui.PromptDefault("BBR Profile (standard/conservative/aggressive)", defaultString(server.BBRProfile, "standard"))
		server.Realm = configureRealm(server.Realm)
	}
	if protocol == "shadowtls" {
		key := server.ShadowsocksKey
		if key == "" {
			var raw [32]byte
			if _, err := rand.Read(raw[:]); err != nil {
				tui.Error(err.Error())
				return false
			}
			key = base64.StdEncoding.EncodeToString(raw[:])
		}
		server.ShadowsocksKey = managedSecret("SS2022 Base64 Key", server.ShadowsocksKey, key)
	} else {
		var ok bool
		server.Certificate, server.Key, ok = managedCertificate(*n, server.ServerName, server.Certificate, server.Key)
		if !ok {
			return false
		}
	}
	if protocol != "shadowtls" && tui.Confirm("Enable ECH (Public Configuration Travels In Setup Link)", server.ECHKey != "") {
		if server.ECHKey == "" {
			publicName := tui.PromptDefault("ECH Public DNS Name", server.ServerName)
			public, private, err := naive.GenerateECH(context.Background(), server.Binary, publicName)
			if err != nil {
				tui.Error(err.Error())
				return false
			}
			server.ECHConfig, server.ECHKey = public, private
		}
	} else {
		server.ECHConfig, server.ECHKey = "", ""
	}
	n.SingServer = server
	return true
}

func configureRealm(old config.HysteriaRealm) config.HysteriaRealm {
	if !tui.Confirm("Use An Existing Hysteria Realm (NAT Traversal)", old.Enabled()) {
		return config.HysteriaRealm{}
	}
	old.ServerURL = tui.PromptDefault("Realm HTTPS URL", old.ServerURL)
	old.RealmID = tui.PromptDefault("Realm ID (Same On Both Peers)", old.RealmID)
	old.Token = managedSecret("Realm Account Token", old.Token, "")
	old.STUNServers = tui.PromptDefault("STUN Endpoints (Comma-Separated Host:Port)", old.STUNServers)
	return old
}
