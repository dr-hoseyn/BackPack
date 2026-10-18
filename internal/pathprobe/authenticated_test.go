package pathprobe

import (
	"net"
	"testing"
	"time"
)

func TestAuthenticationSurvivesSubsequentPayloadFailure(t *testing.T) {
	const token = "private-token"
	_, request, err := Request(token, 1024)
	if err != nil {
		t.Fatal(err)
	}
	for _, valid := range []bool{true, false} {
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			proof := mac(token, "server", request)
			if !valid {
				proof[0] ^= 255
			}
			b.Write(proof)
		}()
		r, err := Check(a, token, request, time.Now())
		a.Close()
		if err == nil || r.Authenticated != valid {
			t.Fatalf("proof=%v result=%+v err=%v", valid, r, err)
		}
	}
}
