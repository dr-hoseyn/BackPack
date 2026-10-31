package health

import (
	"github.com/backpack/backpack/config"
	"testing"
	"time"
)

func TestKernelHealthCacheIsSpecificToPeerAndCredentialGeneration(t *testing.T) {
	c := config.KernelConfig{Interface: "fixture", LocalIP: "10.77.1.1/30", PeerIP: "10.77.1.2", Token: "credential-a"}
	key := c.Interface + "/" + c.LocalIP + "/" + c.PeerIP + "/" + c.Token
	kernelObservations.Lock()
	kernelObservations.items[key] = kernelObservation{at: time.Now(), reachable: true}
	kernelObservations.Unlock()
	defer func() { kernelObservations.Lock(); delete(kernelObservations.items, key); kernelObservations.Unlock() }()
	if !kernelPeerReachable(c) {
		t.Fatal("lost current verified peer observation")
	}
	changed := c
	changed.Token = "credential-b"
	if kernelPeerReachable(changed) {
		t.Fatal("a rekeyed generation reused the old peer's successful observation")
	}
	changed = c
	changed.PeerIP = "10.77.1.3"
	if kernelPeerReachable(changed) {
		t.Fatal("a different peer reused the old reachability observation")
	}
}
