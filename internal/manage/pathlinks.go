package manage

import (
	"errors"
	"fmt"
	"net"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/app"
)

func pathServerEndpoint(p config.ManagedServerPath) string {
	if p.Short.Enabled() {
		return p.Short.Listen
	}
	if p.Sing.Enabled() {
		return p.Sing.Listen
	}
	if p.Xray.Enabled() {
		return p.Xray.Listen
	}
	return p.Naive.Listen
}
func pathClientEndpoint(p config.ManagedClientPath) string {
	if p.Short.Enabled() {
		return p.Short.Server
	}
	if p.Sing.Enabled() {
		return p.Sing.Server
	}
	if p.Xray.Enabled() {
		return p.Xray.Server
	}
	return p.Naive.Server
}

func managedPathsLink(l ShareLink, server config.ServerConfig) (string, error) {
	l.Tr, l.InnerPort = "managed-paths", l.Port
	for _, p := range server.Paths {
		child := server
		child.Paths = nil
		child.Naive = p.Naive
		child.Xray = p.Xray
		child.Sing = p.Sing
		child.Short = p.Short
		if server.NativeSing().NativeUDP && !p.Sing.NativeUDP || p.Short.Enabled() {
			off := false
			child.AcceptUDP = &off
		}
		host := l.Host
		var port string
		if p.Endpoint != "" {
			var err error
			host, port, err = net.SplitHostPort(p.Endpoint)
			if err != nil {
				return "", err
			}
		}
		raw, err := shareLinkOf(p.Name, host, config.Config{Server: child})
		if err != nil {
			return "", err
		}
		sub, err := DecodeShareLink(raw)
		if err != nil {
			return "", err
		}
		if port != "" {
			sub.Port = port
			raw, err = sub.Encode()
			if err != nil {
				return "", err
			}
		}
		if l.Host == "" {
			l.Host = host
		}
		l.PathLinks = append(l.PathLinks, raw)
	}
	return l.Encode()
}

func decodeManagedPaths(l ShareLink) ([]ShareLink, error) {
	if l.Kind != "reverse" || l.From != "iran" || !validPort(l.InnerPort) || len(l.PathLinks) < 2 || len(l.PathLinks) > 8 || len(l.Fallbacks) != 0 || len(l.Hosts) != 0 {
		return nil, errors.New("invalid managed paths setup link")
	}
	paths := make([]ShareLink, 0, len(l.PathLinks))
	names := map[string]bool{}
	for _, raw := range l.PathLinks {
		p, err := DecodeShareLink(raw)
		if err != nil {
			return nil, err
		}
		if p.Tr == "managed-paths" || len(p.PathLinks) != 0 || !managedTransport(p.Tr) || p.Kind != "reverse" || p.From != "iran" || p.InnerPort != l.InnerPort || p.Tok != l.Tok || p.Name == "" || names[p.Name] || !validPort(p.Port) {
			return nil, errors.New("managed path identities, private ports and tokens must match the enclosing link")
		}
		if p.HelperNativeUDP && (!config.SingDatagram(p.Tr) || !l.AcceptUDP) {
			return nil, errors.New("native UDP path requires paired Hysteria2/TUIC and UDP forwarding")
		}
		names[p.Name] = true
		paths = append(paths, p)
	}
	return paths, nil
}

func managedPathsClientFromLink(s *TunnelSpec, l ShareLink, host string) {
	s.ClientPaths = nil
	paths, err := decodeManagedPaths(l)
	if err != nil {
		return
	}
	for _, p := range paths {
		endpointHost := p.Host
		if endpointHost == "" {
			endpointHost = host
		}
		child := *s
		clearManagedTransport(&child)
		managedClientFromLink(&child, p, endpointHost)
		s.ClientPaths = append(s.ClientPaths, config.ManagedClientPath{Name: p.Name, Naive: child.NaiveClient, Xray: child.XrayClient, Sing: child.SingClient, Short: child.ShortClient})
	}
	s.ConnectionPool = 8
}

func prepareManagedPaths(s TunnelSpec, l ShareLink) error {
	paths, err := decodeManagedPaths(l)
	if err != nil {
		return err
	}
	withoutCA := s
	withoutCA.ClientPaths = append([]config.ManagedClientPath(nil), s.ClientPaths...)
	for i := range withoutCA.ClientPaths {
		p := &withoutCA.ClientPaths[i]
		p.Naive.CAFile = ""
		p.Xray.CAFile = ""
		p.Xray.DownloadCAFile = ""
		p.Sing.CAFile = ""
		p.Short.CAFile = ""
	}
	if err := validateManagedSpec(withoutCA); err != nil {
		return err
	}
	for _, p := range paths {
		if p.HelperCA == "" {
			continue
		}
		if _, err := managedLinkCertificates(p.HelperCA); err != nil {
			return err
		}
		if err := app.WriteFileAtomic(managedLinkCAPath(p.HelperCA), []byte(p.HelperCA), 0600); err != nil {
			return fmt.Errorf("storing path %s public trust: %w", p.Name, err)
		}
	}
	return validateManagedSpec(s)
}
