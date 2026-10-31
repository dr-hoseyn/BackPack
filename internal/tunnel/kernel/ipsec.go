package kernel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sort"
	"time"

	"github.com/backpack/backpack/config"
)

// IKEv2 owns SA keys, counters and rekey. Installing a static AES-GCM XFRM SA
// from the tunnel token would reuse nonces after a restart. Only the named
// connection and credential belonging to this tunnel are loaded or removed.
// The local daemon needs strongSwan's vici, kernel-netlink and PSK plugins.
type viciMessage map[string]any

func viciEncode(m viciMessage) []byte {
	var b bytes.Buffer
	name := func(t byte, key string) { b.WriteByte(t); b.WriteByte(byte(len(key))); b.WriteString(key) }
	value := func(v string) { _ = binary.Write(&b, binary.BigEndian, uint16(len(v))); b.WriteString(v) }
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch v := m[key].(type) {
		case string:
			name(3, key)
			value(v)
		case []string:
			name(4, key)
			for _, item := range v {
				b.WriteByte(5)
				value(item)
			}
			b.WriteByte(6)
		case viciMessage:
			name(1, key)
			b.Write(viciEncode(v))
			b.WriteByte(2)
		}
	}
	return b.Bytes()
}

func viciRequest(ctx context.Context, cmd string, msg viciMessage) error {
	c, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", "/var/run/charon.vici")
	if err != nil {
		return fmt.Errorf("kernel IPsec requires a running strongSwan charon daemon with VICI: %w", err)
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	body := append([]byte{0, byte(len(cmd))}, []byte(cmd)...)
	body = append(body, viciEncode(msg)...)
	var packet bytes.Buffer
	_ = binary.Write(&packet, binary.BigEndian, uint32(len(body)))
	packet.Write(body)
	if _, err = io.Copy(c, &packet); err != nil {
		return err
	}
	var length uint32
	if err = binary.Read(c, binary.BigEndian, &length); err != nil {
		return err
	}
	if length < 1 || length > 512<<10 {
		return fmt.Errorf("kernel IPsec: invalid VICI reply")
	}
	reply := make([]byte, length)
	if _, err = io.ReadFull(c, reply); err != nil {
		return err
	}
	if reply[0] != 1 {
		return fmt.Errorf("kernel IPsec: unsupported VICI command %s", cmd)
	}
	// These non-streaming commands return root string pairs only. Parse them
	// strictly, so malformed or truncated replies cannot authorize startup.
	fields := map[string]string{}
	r := bytes.NewReader(reply[1:])
	for r.Len() > 0 {
		t, e := r.ReadByte()
		if e != nil || t != 3 {
			return fmt.Errorf("kernel IPsec: malformed VICI result")
		}
		n, e := r.ReadByte()
		if e != nil {
			return e
		}
		key := make([]byte, n)
		if _, e = io.ReadFull(r, key); e != nil {
			return e
		}
		var size uint16
		if e = binary.Read(r, binary.BigEndian, &size); e != nil {
			return e
		}
		value := make([]byte, size)
		if _, e = io.ReadFull(r, value); e != nil {
			return e
		}
		fields[string(key)] = string(value)
	}
	if fields["success"] != "yes" {
		return fmt.Errorf("kernel IPsec: %s refused: %s", cmd, fields["errmsg"])
	}
	return nil
}

func ipsec(ctx context.Context, c config.KernelConfig, local, remote string) (func(), error) {
	name := "backpack-" + c.Interface
	identityHash := sha256.Sum256([]byte("backpack-kernel-ipsec-v1|" + c.Token))
	identity := fmt.Sprintf("backpack-%x", identityHash[:8])
	ours, theirs := identity+"-"+c.Role, identity+"-iran"
	if c.Role == "iran" {
		theirs = identity + "-kharej"
	}
	child := name + "-gre"
	conn := viciMessage{"version": "2", "local_addrs": []string{local}, "remote_addrs": []string{remote}, "proposals": []string{"aes256gcm16-prfsha256-curve25519"}, "rekey_time": "3600", "dpd_delay": "20", "local": viciMessage{"auth": "psk", "id": ours}, "remote": viciMessage{"auth": "psk", "id": theirs}, "children": viciMessage{child: viciMessage{"mode": "transport", "local_ts": []string{local + "/32[gre]"}, "remote_ts": []string{remote + "/32[gre]"}, "esp_proposals": []string{"aes256gcm16"}, "start_action": "trap", "dpd_action": "restart", "rekey_time": "1800"}}}
	loadedConn, loadedKey := false, false
	cleanup := func() {
		clean, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if loadedConn {
			_ = viciRequest(clean, "uninstall", viciMessage{"ike": name, "child": child})
			_ = viciRequest(clean, "terminate", viciMessage{"ike": name, "timeout": "1000", "force": "yes"})
			_ = viciRequest(clean, "unload-conn", viciMessage{"name": name})
		}
		if loadedKey {
			_ = viciRequest(clean, "unload-shared", viciMessage{"id": name})
		}
	}
	if err := viciRequest(ctx, "load-shared", viciMessage{"id": name, "type": "IKE", "data": c.Token, "owners": []string{ours, theirs}}); err != nil {
		return nil, err
	}
	loadedKey = true
	if err := viciRequest(ctx, "load-conn", viciMessage{name: conn}); err != nil {
		cleanup()
		return nil, err
	}
	loadedConn = true
	// Trap policies prevent GRE from escaping unencrypted before IKE completes.
	if err := viciRequest(ctx, "install", viciMessage{"ike": name, "child": child}); err != nil {
		cleanup()
		return nil, err
	}
	return cleanup, nil
}
