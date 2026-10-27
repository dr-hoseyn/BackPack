package network

import (
	"net/http"
	"strconv"
	"testing"
)

func TestWSMuxNegotiationAndLegacyFallback(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		server, client, want int
	}{
		{"auto", 0, 0, 1}, {"old preference", 0, 1, 1},
		{"server v1", 1, 0, 1}, {"server v2", 2, 0, 2},
		{"both v1", 1, 1, 1}, {"both v2", 2, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := WSMuxOptions{Version: tc.client, PoolSize: 8}
			h := make(http.Header)
			o.offer(h)
			v, floor, err := SelectWSMux(h, tc.server)
			if err != nil || v != tc.want || floor != 8 {
				t.Fatalf("selection = %d/%d, %v", v, floor, err)
			}
			response := http.Header{}
			response.Set(WSMuxVersionHeader, strconv.Itoa(v))
			response.Set(WSMuxPoolHeader, "8")
			if err := o.accept(response); err != nil || o.NegotiatedVersion != v || !o.Retirement {
				t.Fatalf("client negotiation: %+v, %v", o, err)
			}
		})
	}
	for _, configured := range []int{0, 1, 2} {
		v, floor, err := SelectWSMux(http.Header{}, configured)
		if err != nil || v != ResolveStaticMuxVersion(configured) || floor != 0 {
			t.Fatal("old client no longer gets its static protocol")
		}
		o := WSMuxOptions{Version: configured, PoolSize: 8}
		if err := o.accept(http.Header{}); err != nil || o.NegotiatedVersion != v || o.Retirement {
			t.Fatal("old server no longer gets its static protocol")
		}
	}
}

func TestWSMuxRejectsIncompatibleOrMalformedNegotiation(t *testing.T) {
	for _, offer := range []string{"0", "3", "2,garbage", "", "1"} {
		h := http.Header{}
		h.Set(WSMuxOfferHeader, offer)
		h.Set(WSMuxPoolHeader, "-1")
		if _, _, err := SelectWSMux(h, 2); offer != "" && err == nil {
			t.Fatalf("accepted %q", offer)
		}
	}
	for _, value := range []string{"-1", "65537", "many", "0"} {
		h := http.Header{}
		h.Set(WSMuxOfferHeader, "2,1")
		h.Set(WSMuxPoolHeader, value)
		if _, _, err := SelectWSMux(h, 0); err == nil {
			t.Fatalf("accepted invalid pool floor %q", value)
		}
	}
	for _, value := range []string{"0", "3", "1", "no"} {
		o := WSMuxOptions{Version: 2, PoolSize: 8}
		h := http.Header{}
		h.Set(WSMuxVersionHeader, value)
		if err := o.accept(h); err == nil {
			t.Fatalf("accepted unoffered version %q", value)
		}
	}
}
