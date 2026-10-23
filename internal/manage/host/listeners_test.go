package host

import (
	"net"
	"reflect"
	"testing"
)

func TestTunnelPortChecksUseTheActualListenerProtocol(t *testing.T) {
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	addr := tcp.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	if !PortInUse(port) || !TunnelPortInUse("tcp", addr) || !TunnelPortInUse("pck", addr) {
		t.Fatal("live TCP listener was not detected")
	}
	if TunnelPortInUse("udp", addr) {
		t.Fatal("TCP listener incorrectly blocks a UDP tunnel on the same port")
	}
	udp, err := net.ListenPacket("udp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	if !TunnelPortInUse("udp", addr) || !TunnelPortInUse("udp", port) {
		t.Fatal("live UDP listener was not detected")
	}
	if !TunnelPortInUse("tcp", "invalid:address:port") || !TunnelPortInUse("udp", "invalid:address:port") {
		t.Fatal("invalid listener address was reported as usable")
	}
	if err := tcp.Close(); err != nil {
		t.Fatal(err)
	}
	if PortInUse(port) || TunnelPortInUse("tcp", addr) {
		t.Fatal("UDP listener incorrectly blocks a TCP tunnel on the same port")
	}
}

func TestCLIHostChecksReadConfiguredTunnelBindings(t *testing.T) {
	old := listTunnels
	listTunnels = func() []tunnel {
		return []tunnel{{Name: "existing", Role: "server", Addr: "127.0.0.1:31001", Ports: []string{"31002=127.0.0.1:443", "31003-31004"}}}
	}
	t.Cleanup(func() { listTunnels = old })
	if got := ReservedPorts(); !reflect.DeepEqual(got, []int{31001, 31002, 31003, 31004}) {
		t.Fatalf("reserved bindings include remote ports or miss local ports: %v", got)
	}
	if PortClash("server", "127.0.0.1:31001", "new") == "" {
		t.Fatal("CLI allowed a conflicting configured listener")
	}
}
