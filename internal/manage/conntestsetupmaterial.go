package manage

// Promotion reads each endpoint's own tested config and keeps its private
// identities on that machine. Only the selected case and a fresh shared secret
// cross the coordinator. Temporary certificate paths are copied into a private
// owned directory before the test workspace is removed.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/app"
	"github.com/backpack/backpack/internal/tunnel/kernel"
	"github.com/backpack/backpack/internal/tunnel/l3"
)

type ctSetupMaterial struct {
	cases  map[string]config.Config
	assets map[string][]byte
	root   string
}

func ctSetupCaseID(c ShareLink) string { return c.Kind + "/" + c.Tr + "/" + c.SNI }

var ctSetupMaterials = struct {
	sync.Mutex
	items map[string]*ctSetupMaterial
}{items: map[string]*ctSetupMaterial{}}

func ctMaterialID(a connTestAddr) string { return a.Host + ":" + fmt.Sprint(a.Coord) + "/" + a.Tok }

func ctGetSetupMaterial(a connTestAddr) *ctSetupMaterial {
	ctSetupMaterials.Lock()
	defer ctSetupMaterials.Unlock()
	return ctSetupMaterials.items[ctMaterialID(a)]
}

func ctCanSetup(raw string) bool {
	a, err := parseConnTestLink(raw)
	if err != nil {
		return false
	}
	if len(a.Routes) == 0 {
		return ctGetSetupMaterial(a) != nil
	}
	for _, route := range a.Routes {
		if ctGetSetupMaterial(route) != nil {
			return true
		}
	}
	return false
}

func ctRememberSetupMaterial(link ConnTestLink, dir string) {
	m, err := ctCaptureSetupMaterial(link, dir)
	if err != nil {
		return
	} // failed captures are reported if automatic setup is requested
	id := ctMaterialID(connTestAddr{Host: link.Host, Coord: link.Coord, Tok: link.Tok})
	ctSetupMaterials.Lock()
	ctSetupMaterials.items[id] = m
	ctSetupMaterials.Unlock()
	time.AfterFunc(time.Until(time.Unix(link.Until, 0))+ConnTestSetupWindow+time.Minute, func() {
		ctSetupMaterials.Lock()
		if ctSetupMaterials.items[id] == m {
			delete(ctSetupMaterials.items, id)
		}
		ctSetupMaterials.Unlock()
	})
}

func ctWalkStrings(v reflect.Value, visit func(string) (string, error)) error {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			return ctWalkStrings(v.Elem(), visit)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).CanSet() {
				if err := ctWalkStrings(v.Field(i), visit); err != nil {
					return err
				}
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := ctWalkStrings(v.Index(i), visit); err != nil {
				return err
			}
		}
	case reflect.String:
		value, err := visit(v.String())
		if err != nil {
			return err
		}
		v.SetString(value)
	}
	return nil
}

func ctCaptureSetupMaterial(link ConnTestLink, dir string) (*ctSetupMaterial, error) {
	m := &ctSetupMaterial{cases: map[string]config.Config{}, assets: map[string][]byte{}, root: filepath.Join(app.ConfigDir, "setup")}
	for _, c := range link.Cases {
		if !validName(c.Name) {
			continue
		}
		var cfg config.Config
		if _, err := toml.DecodeFile(filepath.Join(dir, c.Name+".toml"), &cfg); err != nil {
			continue
		}
		err := ctWalkStrings(reflect.ValueOf(&cfg).Elem(), func(value string) (string, error) {
			if value == "" || !filepath.IsAbs(value) {
				return value, nil
			}
			rel, err := filepath.Rel(dir, value)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return value, nil
			}
			st, err := os.Lstat(value)
			if err != nil || !st.Mode().IsRegular() || st.Size() > 256<<10 {
				return "", fmt.Errorf("cannot retain a private test asset")
			}
			body, err := os.ReadFile(value)
			if err != nil {
				return "", err
			}
			m.assets[value] = body
			return value, nil
		})
		if err == nil {
			m.cases[c.Name] = cfg
			m.cases[ctSetupCaseID(c)] = cfg
		}
	}
	return m, nil
}

func ctSetupAWGKey(secret string) string {
	sum := sha256.Sum256([]byte("backpack-permanent-awg/" + secret))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func ctSetupOwner(secret string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(secret))) }

func (m *ctSetupMaterial) prepare(f PeerForm) (PeerForm, error) {
	if !validName(f.Name) || f.Token == "" {
		return f, fmt.Errorf("invalid permanent tunnel identity")
	}
	original, ok := m.cases[f.TestCase]
	if !ok {
		return f, fmt.Errorf("the selected tested configuration is unavailable on this server")
	}
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(original); err != nil {
		return f, err
	}
	var cfg config.Config
	if _, err := toml.Decode(encoded.String(), &cfg); err != nil {
		return f, err
	}
	if (cfg.Kernel.Enabled() && (f.Kind != "kernel" || cfg.Kernel.Role != f.Side)) || (cfg.AWG.Enabled() && f.Kind != "awg") || (cfg.L3.Enabled() && f.Kind != "direct") ||
		(cfg.Server.BindAddr != "" && (f.Kind != "reverse" || f.Side != "iran")) ||
		(cfg.Client.RemoteAddr != "" && (f.Kind != "reverse" || f.Side != "kharej")) {
		return f, fmt.Errorf("tested engine does not match the requested side")
	}
	f.SetupAssetDir = filepath.Join(m.root, f.Name)
	_, existed := os.Stat(f.SetupAssetDir)
	if err := os.MkdirAll(f.SetupAssetDir, 0700); err != nil {
		return f, err
	}
	owner := filepath.Join(f.SetupAssetDir, "owner")
	if data, err := os.ReadFile(owner); (err == nil && string(data) != ctSetupOwner(f.Token)) || (err != nil && existed == nil) {
		return f, fmt.Errorf("setup asset directory belongs to another tunnel")
	}
	if err := app.WriteFileAtomic(owner, []byte(ctSetupOwner(f.Token)), 0600); err != nil {
		return f, err
	}
	err := ctWalkStrings(reflect.ValueOf(&cfg).Elem(), func(value string) (string, error) {
		data, exists := m.assets[value]
		if !exists {
			return value, nil
		}
		sum := sha256.Sum256([]byte(value))
		path := filepath.Join(f.SetupAssetDir, fmt.Sprintf("%x-%s", sum[:6], filepath.Base(value)))
		return path, app.WriteFileAtomic(path, data, 0600)
	})
	if err != nil {
		return f, err
	}
	if cfg.Kernel.Enabled() {
		method, reverse, ok := ctKernelMethod(f.Transport)
		if !ok || method.Name != cfg.Kernel.Method || reverse != cfg.Kernel.Reverse {
			return f, fmt.Errorf("tested kernel method or direction does not match the selection")
		}
		cfg.Kernel.Token = f.Token
		cfg.Kernel.ID = 0
		cfg.Kernel.Interface = ""
		cfg.Kernel.Ports = nil
		cfg.Kernel.AcceptUDP = false
		if f.Side == "iran" {
			cfg.Kernel.Ports = parsePorts(f.Ports)
			cfg.Kernel.AcceptUDP = f.AcceptUDP
		}
		var err error
		cfg.Kernel, err = kernel.Resolve(cfg.Kernel)
		if err != nil {
			return f, err
		}
	} else if cfg.AWG.Enabled() {
		cfg.AWG.Interface = "bpawg" + fmt.Sprintf("%x", sha256.Sum256([]byte(f.Name)))[:8]
		cfg.AWG.PresharedKey = ctSetupAWGKey(f.Token)
		cfg.AWG.Obfuscation.HeaderProtectionKey = ctSetupAWGKey("headers/" + f.Token)
		cfg.AWG.Ports = nil
		if f.Side == "iran" {
			var err error
			cfg.AWG.Ports, err = ctAWGForwardPorts(f.Ports, hostOf(cfg.AWG.PeerAddress))
			if err != nil {
				return f, err
			}
			cfg.AWG.AcceptUDP = f.AcceptUDP
		}
	} else if cfg.L3.Enabled() {
		cfg.L3.Token = f.Token
		cfg.L3.Iface = freeL3Iface()
		cfg.L3.Ports = nil
		if f.Side == "iran" {
			cfg.L3.Ports = parsePorts(f.Ports)
			cfg.L3.AcceptUDP = f.AcceptUDP
		}
	} else if cfg.Server.BindAddr != "" {
		cfg.Server.Token = f.Token
		cfg.Server.SkipOptz = false
		cfg.Server.Ports = parsePorts(f.Ports)
		udp := f.AcceptUDP
		cfg.Server.AcceptUDP = &udp
	} else if cfg.Client.RemoteAddr != "" {
		cfg.Client.Token = f.Token
		cfg.Client.SkipOptz = false
	} else {
		return f, fmt.Errorf("the selected configuration has no supported tunnel engine")
	}
	// Rotate paired helper credentials, preserving each host's independent
	// certificate/REALITY identity and the verified cover, mode and endpoint.
	helperID := fmt.Sprintf("%x", sha256.Sum256([]byte("backpack-setup-helper/"+f.Token)))
	for _, target := range []any{&cfg.Server.Naive, &cfg.Client.Naive, &cfg.Server.Xray, &cfg.Client.Xray, &cfg.Server.Sing, &cfg.Client.Sing, &cfg.Server.Connect, &cfg.Client.Connect} {
		v := reflect.ValueOf(target).Elem()
		for _, key := range []string{"Password", "ObfsPassword", "UUID", "ShadowsocksKey", "ShortID", "Path"} {
			field := v.FieldByName(key)
			if !field.IsValid() || field.Kind() != reflect.String || field.String() == "" {
				continue
			}
			value := f.Token
			switch key {
			case "UUID":
				value = ctUUID(helperID[:32])
			case "ShadowsocksKey":
				value = ctSetupAWGKey(f.Token)
			case "ShortID":
				value = helperID[:16]
			case "Path":
				value = "/" + f.Token
			}
			field.SetString(value)
		}
	}
	f.SetupConfig = &cfg
	return f, nil
}

func ctAWGForwardPorts(raw, peer string) ([]string, error) {
	ports := parsePorts(raw)
	for i, port := range ports {
		listen, target, mapped := strings.Cut(port, "=")
		if !mapped {
			continue
		}
		backends := strings.Split(target, "|")
		for j, backend := range backends {
			h, p, err := net.SplitHostPort(strings.TrimSpace(backend))
			if err != nil {
				continue
			}
			if h == "localhost" || h == "127.0.0.1" || h == "::1" {
				h = peer
			}
			if h != "" && h != peer {
				return nil, fmt.Errorf("AmneziaWG backends must listen on the peer tunnel address or wildcard")
			}
			backends[j] = net.JoinHostPort(h, p)
		}
		ports[i] = listen + "=" + strings.Join(backends, "|")
	}
	return ports, validatePortSpecs(ports)
}

func ctRemoveSetupAssets(f PeerForm) error {
	if f.SetupAssetDir == "" {
		return nil
	}
	if !validName(f.Name) || filepath.Base(f.SetupAssetDir) != f.Name {
		return fmt.Errorf("invalid setup asset ownership")
	}
	owner, err := os.ReadFile(filepath.Join(f.SetupAssetDir, "owner"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if string(owner) != ctSetupOwner(f.Token) {
		return fmt.Errorf("setup asset ownership changed")
	}
	return os.RemoveAll(f.SetupAssetDir)
}

func ctValidatePromotedForm(f PeerForm) error {
	c := f.SetupConfig
	if c.Kernel.Enabled() {
		cfg, err := kernel.Resolve(c.Kernel)
		if err != nil {
			return err
		}
		if err = ctKernelPrerequisite(cfg); err != nil {
			return err
		}
		if busy := busyForwardPorts(cfg.Ports, cfg.PeerIP); len(busy) > 0 {
			return fmt.Errorf("forwarded ports in use: %v", busy)
		}
		return nil
	}
	if c.AWG.Enabled() {
		if _, err := awgBody(c.AWG); err != nil {
			return err
		}
		if busy := busyForwardPorts(c.AWG.Ports, hostOf(c.AWG.PeerAddress)); len(busy) > 0 {
			return fmt.Errorf("forwarded ports in use: %v", busy)
		}
		if c.AWG.ListenPort > 0 && TunnelPortInUse("udp", fmt.Sprintf(":%d", c.AWG.ListenPort)) {
			return fmt.Errorf("AmneziaWG UDP port is busy")
		}
		return nil
	}
	if c.L3.Enabled() {
		s, err := l3.ConfigFromTable(c.L3)
		if err != nil {
			return err
		}
		if err := s.Validate(); err != nil {
			return err
		}
		if busy := busyForwardPorts(c.L3.Ports, c.L3.PeerIP); len(busy) > 0 {
			return fmt.Errorf("forwarded ports in use: %v", busy)
		}
		return nil
	}
	if c.Server.BindAddr != "" {
		if err := validatePortSpecs(c.Server.Ports); err != nil {
			return err
		}
		if busy := reverseBusyPorts(c.Server.Ports, c.Server.BindAddr, string(c.Server.Transport)); len(busy) > 0 {
			return fmt.Errorf("forwarded ports in use: %v", busy)
		}
	}
	return nil
}

func ctApplySetupForm(f PeerForm) (string, bool, error) {
	if f.SetupConfig == nil {
		return applyPeerForm(f)
	}
	if fileExists(app.ConfigPath(f.Name)) {
		return "", false, fmt.Errorf("tunnel %s already exists", f.Name)
	}
	if f.SetupConfig.Kernel.Enabled() {
		result, err := installKernel(f.Name, f.SetupConfig.Kernel)
		return result.Service, result.Active, err
	}
	var body bytes.Buffer
	if err := toml.NewEncoder(&body).Encode(f.SetupConfig); err != nil {
		return "", false, err
	}
	if err := writeDirectConfig(f.Name, body.String()); err != nil {
		return "", false, err
	}
	service := app.ServiceName(f.Name)
	if err := StartService(service); err != nil {
		return service, false, err
	}
	return service, WaitServiceActive(service, 10*time.Second), nil
}
