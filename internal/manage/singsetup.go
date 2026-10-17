package manage

import (
	"crypto/rand"
	"encoding/base64"
	"strings"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tui"
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
			client.ObfsPassword = managedSecret("Salamander Password (Empty = Disabled)", client.ObfsPassword, "")
		}
		if protocol == "shadowtls" {
			client.ShadowsocksKey = managedSecret("SS2022 Base64 Key (From Iran)", client.ShadowsocksKey, "")
		}
		client.CAFile = tui.PromptDefault("Private CA File (Empty = System Trust)", client.CAFile)
		n.SingClient = client
		return true
	}
	server.Protocol, server.Listen = protocol, public
	server.Binary = tui.PromptDefault("Sing-Box Binary", defaultString(server.Binary, managedHelperPath("sing-box")))
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
		if tui.Confirm("Enable Salamander Obfuscation (Both Sides Must Match)", server.ObfsPassword != "") {
			server.ObfsPassword = managedSecret("Salamander Password", server.ObfsPassword, randomToken(32))
		} else {
			server.ObfsPassword = ""
		}
	}
	if protocol == "shadowtls" {
		server.Handshake = strings.TrimSpace(tui.PromptDefault("ShadowTLS Cover Endpoint (Hostname:Port)", server.Handshake))
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
	n.SingServer = server
	return true
}
