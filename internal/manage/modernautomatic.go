package manage

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tunnel/naive"
)

// Keep the warm pool bounded; operators can explicitly choose any 2..8
// compatible carriers. Experimental Tailcat is always an explicit selection.
func modernAutomaticCarriers(available []string) []string {
	eligible := map[string]bool{"trusttunnel": true, "webtransport": true}
	for _, v := range available {
		eligible[v] = true
	}
	if eligible["reality"] {
		eligible["xhttp-reality"] = true
	}
	var selected []string
	for _, v := range []string{"hysteria2", "trusttunnel", "webtransport", "xhttp-reality", "anytls", "naive", "tuic", "short-https", "xhttp", "reality", "shadowtls"} {
		if eligible[v] {
			selected = append(selected, v)
			if len(selected) == 8 {
				break
			}
		}
	}
	return selected
}

func customAutomaticCarriers(raw, derp, cover string, sing, xray, naiveInstalled bool) ([]string, error) {
	var selected []string
	seen := map[string]bool{}
	for _, v := range strings.Split(raw, ",") {
		v = strings.TrimSpace(strings.ToLower(v))
		if v == "" || seen[v] {
			return nil, errors.New("carrier list contains an empty or repeated entry")
		}
		seen[v] = true
		switch v {
		case "short-https", "trusttunnel", "webtransport":
		case "xhttp", "reality", "xhttp-reality":
			if !xray {
				return nil, errors.New("selected carrier requires the pinned Xray helper")
			}
		case "naive":
			if !naiveInstalled || !sing {
				return nil, errors.New("Naive requires both pinned client and server helpers")
			}
		case "hysteria2", "tuic", "anytls", "shadowtls":
			if !sing {
				return nil, errors.New("selected carrier requires the pinned stable sing-box helper")
			}
		case "tailcat":
			if !automaticExecutable(managedHelperPath("tailcat")) {
				return nil, errors.New("install the optional separate Tailcat helper on both peers")
			}
			u, err := url.Parse(derp)
			if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
				return nil, errors.New("Tailcat requires an explicit verified HTTPS DERP map URL")
			}
		default:
			return nil, fmt.Errorf("unsupported automatic carrier %q", v)
		}
		if (v == "reality" || v == "xhttp-reality" || v == "shadowtls") && cover == "" {
			return nil, errors.New("selected cover carrier requires an authenticated verified cover")
		}
		selected = append(selected, v)
	}
	if len(selected) < 2 || len(selected) > 8 {
		return nil, errors.New("choose 2..8 independent carriers")
	}
	return selected, nil
}

func automaticTailcat(listen string) (config.SingServerConfig, error) {
	x := config.SingServerConfig{Protocol: "tailcat", Binary: managedHelperPath("tailcat"), Listen: listen}
	for _, key := range []*string{&x.Tailcat.PrivateKey, &x.Tailcat.ClientKey, &x.Tailcat.PreSharedKey} {
		v, err := naive.NewTailcatKey()
		if err != nil {
			return x, err
		}
		*key = v
	}
	return x, nil
}
