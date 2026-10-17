package adaptive

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tunnel/naive"
)

var pathName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$`)

func serverConfig(base config.ServerConfig, p config.ManagedServerPath) config.ServerConfig {
	native := base.NativeSing().NativeUDP
	base.Paths = nil
	base.Naive = p.Naive
	base.Xray = p.Xray
	base.Sing = p.Sing
	base.Short = p.Short
	if native || p.Short.Enabled() {
		off := false
		base.AcceptUDP = &off
	}
	if p.Sing.NativeUDP {
		on := true
		base.AcceptUDP = &on
	}
	return base
}
func clientConfig(base config.ClientConfig, p config.ManagedClientPath) config.ClientConfig {
	base.Paths = nil
	base.PathPolicy = config.PathPolicy{}
	base.Naive = p.Naive
	base.Xray = p.Xray
	base.Sing = p.Sing
	base.Short = p.Short
	return base
}

func count(v ...bool) int {
	n := 0
	for _, b := range v {
		if b {
			n++
		}
	}
	return n
}

func Validate(c *config.Config) error {
	s, cl := len(c.Server.Paths), len(c.Client.Paths)
	if s == 0 && cl == 0 {
		if c.Client.PathPolicy != (config.PathPolicy{}) {
			return errors.New("path_policy requires client.paths")
		}
		return nil
	}
	if s != 0 && cl != 0 || c.AWG.Enabled() || c.L3.Enabled() || c.Direct.Enabled() {
		return errors.New("managed paths require one exclusive Reverse role")
	}
	n := s
	if cl != 0 {
		n = cl
	}
	if n < 2 || n > 8 {
		return errors.New("managed paths require 2..8 independent carriers")
	}
	if s != 0 && len(c.Server.Token) < 16 || cl != 0 && len(c.Client.Token) < 16 {
		return errors.New("managed paths require a random reverse token of at least 16 bytes")
	}
	names := map[string]bool{}
	name := func(v string) error {
		if !pathName.MatchString(v) || names[v] {
			return errors.New("path names must be unique 1..32 byte ASCII identities")
		}
		names[v] = true
		return nil
	}
	if s != 0 {
		v := c.Server
		if v.NativeSing().NativeUDP && !v.ForwardsUDP() {
			return errors.New("native UDP paths require server.accept_udp = true")
		}
		if v.BindAddr == "" || c.Client.RemoteAddr != "" || v.Transport != config.TCP || len(v.FallbackTransports) != 0 || v.Naive.Enabled() || v.Xray.Enabled() || v.Sing.Enabled() || v.Short.Enabled() || v.MPTCP {
			return errors.New("server.paths requires TCP Reverse without an inline helper, MPTCP or transport fallback")
		}
		type binding struct{ network, host, port string }
		var bindings []binding
		for _, p := range v.Paths {
			if p.Endpoint != "" {
				host, port, err := net.SplitHostPort(p.Endpoint)
				portNumber, portErr := strconv.Atoi(port)
				if err != nil || host == "" || strings.ContainsAny(host, "/\\\r\n\t @#?") || portErr != nil || portNumber < 1 || portNumber > 65535 {
					return errors.New("path endpoint must be a public host:port")
				}
				if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
					return errors.New("a path's exported endpoint cannot be a wildcard address")
				}
			}
			if err := name(p.Name); err != nil {
				return err
			}
			if count(p.Naive.Enabled(), p.Xray.Enabled(), p.Sing.Enabled(), p.Short.Enabled()) != 1 {
				return fmt.Errorf("path %s requires exactly one managed carrier", p.Name)
			}
			child := config.Config{Server: serverConfig(v, p)}
			if err := validateChild(&child); err != nil {
				return fmt.Errorf("path %s: %w", p.Name, err)
			}
			listen, network := p.Naive.Listen, "tcp"
			if p.Xray.Enabled() {
				listen = p.Xray.Listen
			}
			if p.Short.Enabled() {
				listen = p.Short.Listen
			}
			if p.Sing.Enabled() {
				listen = p.Sing.Listen
				if config.SingDatagram(p.Sing.Protocol) {
					network = "udp"
				}
			}
			host, port, err := net.SplitHostPort(listen)
			if err != nil {
				return err
			}
			for _, b := range bindings {
				if network == b.network && port == b.port && (host == b.host || host == "0.0.0.0" || host == "::" || b.host == "0.0.0.0" || b.host == "::") {
					return errors.New("managed path public listeners overlap")
				}
			}
			bindings = append(bindings, binding{network, host, port})
			if p.Short.Enabled() && v.ForwardsUDP() && !v.NativeSing().NativeUDP {
				return errors.New("a Short HTTPS path requires TCP-only forwarding or separate native UDP paths")
			}
			if p.Sing.NativeUDP {
				native := v.NativeSing()
				if p.Sing.UDPMaxFlows != native.UDPMaxFlows || p.Sing.UDPBandwidthMbps != native.UDPBandwidthMbps {
					return errors.New("native paths share one reverse UDP engine and must use identical local UDP limits")
				}
			}
		}
		return nil
	}
	v := c.Client
	if v.RemoteAddr == "" || c.Server.BindAddr != "" || v.Transport != config.TCP || len(v.FallbackTransports) != 0 || len(v.FallbackAddrs) != 0 || v.Naive.Enabled() || v.Xray.Enabled() || v.Sing.Enabled() || v.Short.Enabled() || v.MPTCP || v.Proxy != "" || v.Interface != "" || v.LocalAddr != "" || v.SOMark != 0 || v.LoadBalance || v.HealthFailover {
		return errors.New("client.paths requires TCP Reverse without an inline helper, routing override or old fallback settings")
	}
	if _, err := policyDefaults(v.PathPolicy); err != nil {
		return err
	}
	endpoints := map[string]bool{}
	for _, p := range v.Paths {
		if err := name(p.Name); err != nil {
			return err
		}
		if count(p.Naive.Enabled(), p.Xray.Enabled(), p.Sing.Enabled(), p.Short.Enabled()) != 1 {
			return fmt.Errorf("path %s requires exactly one managed carrier", p.Name)
		}
		child := config.Config{Client: clientConfig(v, p)}
		if err := validateChild(&child); err != nil {
			return fmt.Errorf("path %s: %w", p.Name, err)
		}
		endpoint := p.Naive.Server
		if p.Xray.Enabled() {
			endpoint = p.Xray.Server
		}
		if p.Short.Enabled() {
			endpoint = p.Short.Server
		}
		if p.Sing.Enabled() {
			endpoint = p.Sing.Protocol + "/" + p.Sing.Server
		}
		endpoint = strings.ToLower(endpoint)
		if endpoints[endpoint] {
			return errors.New("managed paths must use independent public endpoints")
		}
		endpoints[endpoint] = true
		if p.Sing.NativeUDP {
			native := v.NativeSing()
			if p.Sing.UDPMaxFlows != native.UDPMaxFlows || p.Sing.UDPBandwidthMbps != native.UDPBandwidthMbps {
				return errors.New("native paths must use identical local UDP limits")
			}
		}
	}
	return nil
}

func validateChild(c *config.Config) error {
	for _, check := range []func(*config.Config) error{naive.Validate, naive.ValidateXray, naive.ValidateSing, naive.ValidateShort} {
		if err := check(c); err != nil {
			return err
		}
	}
	return nil
}
