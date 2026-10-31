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
	if changed.Interface+"/"+changed.LocalIP+"/"+changed.PeerIP+"/"+changed.Token == key {
		t.Fatal("credential generations collide")
	}
}
