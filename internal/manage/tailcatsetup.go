package manage

import (
	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tui"
	"github.com/backpack/backpack/internal/tunnel/naive"
)

func configureTailcat(n *TunnelSpec, old TunnelSpec, public string) bool {
	tui.Warn("Tailcat is experimental. Install its separate pinned helper. The advertised endpoint is pairing metadata; DERP supplies rendezvous/relay and no public listener is opened there.")
	if n.Role == "client" {
		x := old.SingClient
		if x.Protocol != "tailcat" {
			x = config.SingClientConfig{}
		}
		x.Protocol, x.Server = "tailcat", public
		x.Binary = tui.PromptDefault("Tailcat Sing-Box Binary", defaultString(x.Binary, managedHelperPath("tailcat")))
		x.Tailcat.PrivateKey = managedSecret("Client Private Key From Setup Link", x.Tailcat.PrivateKey, "")
		x.Tailcat.PublicKey = tui.PromptDefault("Server Public Key", x.Tailcat.PublicKey)
		x.Tailcat.DiscoKey = tui.PromptDefault("Server Discovery Public Key", x.Tailcat.DiscoKey)
		x.Tailcat.PreSharedKey = managedSecret("Paired Pre-Shared Key", x.Tailcat.PreSharedKey, "")
		x.Tailcat.DERPMapURL = tui.PromptDefault("Verified HTTPS DERP Map URL", x.Tailcat.DERPMapURL)
		x.Tailcat.DERPRegion = tui.PromptInt("DERP Region ID From The Selected Map (Required)", x.Tailcat.DERPRegion)
		n.SingClient = x
		return true
	}
	x := old.SingServer
	if x.Protocol != "tailcat" {
		x = config.SingServerConfig{}
	}
	x.Protocol, x.Listen = "tailcat", public
	x.Binary = tui.PromptDefault("Tailcat Sing-Box Binary", defaultString(x.Binary, managedHelperPath("tailcat")))
	var err error
	for _, key := range []*string{&x.Tailcat.PrivateKey, &x.Tailcat.ClientKey, &x.Tailcat.PreSharedKey} {
		if *key == "" {
			*key, err = naive.NewTailcatKey()
			if err != nil {
				tui.Error(err.Error())
				return false
			}
		}
	}
	x.Tailcat.DERPMapURL = tui.PromptDefault("Verified HTTPS DERP Map URL (Explicitly Choose Your Relay)", x.Tailcat.DERPMapURL)
	x.Tailcat.DERPRegion = tui.PromptInt("DERP Region ID From The Selected Map (Required)", x.Tailcat.DERPRegion)
	n.SingServer = x
	return true
}
