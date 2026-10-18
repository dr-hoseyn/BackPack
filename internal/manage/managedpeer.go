package manage

import (
	"errors"
	"github.com/backpack/backpack/internal/app"
)

type ManagedPeerPreview struct {
	Name      string   `json:"name"`
	Host      string   `json:"host"`
	Transport string   `json:"transport"`
	Profile   string   `json:"profile"`
	Paths     []string `json:"paths"`
}

// PreviewManagedPeer never writes public trust, credentials or a service. The
// dedicated panel flow creates a new peer; existing peers use reviewed edits.
func PreviewManagedPeer(raw, name string) (ManagedPeerPreview, error) {
	if name != "" && (!validName(name) || fileExists(app.ConfigPath(name))) {
		return ManagedPeerPreview{}, errors.New("choose an unused valid local tunnel name")
	}
	l, err := DecodeShareLink(FindSetupLink(raw))
	if err != nil {
		return ManagedPeerPreview{}, err
	}
	if l.Kind != "reverse" || l.From != "iran" || !managedTransport(l.Tr) {
		return ManagedPeerPreview{}, errors.New("paste an Iran managed HTTPS or Automatic Setup Link")
	}
	if tunnelWithToken(l.Tok) != "" {
		return ManagedPeerPreview{}, errors.New("this peer is already configured; use its existing tunnel's reviewed edit flow")
	}
	s, err := kharejFromLink(l, LinkApplyOptions{Name: name})
	if err != nil {
		return ManagedPeerPreview{}, err
	}
	if fileExists(app.ConfigPath(s.Name)) {
		return ManagedPeerPreview{}, errors.New("choose an unused name")
	}
	v := ManagedPeerPreview{Name: s.Name, Host: l.Host, Transport: l.Tr, Profile: l.PathProfile}
	if l.Tr == "managed-paths" {
		paths, err := decodeManagedPaths(l)
		if err != nil {
			return v, err
		}
		for _, p := range paths {
			v.Paths = append(v.Paths, p.Name+" · "+p.Tr)
		}
	} else {
		v.Paths = []string{l.Tr}
	}
	return v, nil
}

func CreateManagedPeer(raw, name string) (LinkApplied, error) {
	if _, err := PreviewManagedPeer(raw, name); err != nil {
		return LinkApplied{}, err
	}
	return ApplySetupLink(raw, LinkApplyOptions{Name: name})
}
