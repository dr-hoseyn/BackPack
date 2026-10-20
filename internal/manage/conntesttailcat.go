package manage

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/backpack/backpack/internal/app"
	"github.com/backpack/backpack/internal/tunnel/naive"
)

// No public relay is silently selected. The optional helper and the operator's
// explicit DERP selection are the same prerequisites as normal Tailcat setup.
type ctTailcatPeer struct {
	Private string `json:"private"`
	PSK     string `json:"psk"`
	Map     string `json:"map"`
	Region  int    `json:"region"`
	Disco   string `json:"disco"`
}

func ctConfigureTailcatTest(s *TunnelSpec, settings *ctCarrierSettings) error {
	var choice struct {
		Map    string `json:"derp_map_url"`
		Region int    `json:"derp_region"`
	}
	body, err := os.ReadFile(app.ConfigDir + "/conntest-tailcat.json")
	if err != nil || len(body) > 8192 || json.Unmarshal(body, &choice) != nil || choice.Map == "" || choice.Region < 1 {
		return fmt.Errorf("Tailcat needs its optional pinned helper and an explicit DERP selection in /etc/backpack/conntest-tailcat.json")
	}
	x := &s.SingServer.Tailcat
	for _, key := range []*string{&x.PrivateKey, &x.ClientKey, &x.PreSharedKey} {
		*key, err = naive.NewTailcatKey()
		if err != nil {
			return err
		}
	}
	x.DERPMapURL, x.DERPRegion = choice.Map, choice.Region
	public, disco, err := naive.TailcatPublic(x.PrivateKey)
	if err != nil {
		return err
	}
	settings.PublicKey = public
	settings.Tailcat = &ctTailcatPeer{Private: x.ClientKey, PSK: x.PreSharedKey, Map: choice.Map, Region: choice.Region, Disco: disco}
	return nil
}
