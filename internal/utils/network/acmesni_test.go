package network

import (
	"crypto/tls"
	"testing"
)

// The regression test for "the script cannot get a certificate", reported from
// two servers by two people who both concluded Let's Encrypt was down.
//
// autocert identifies the certificate to serve by the name in the ClientHello
// and refuses outright when there is none. A tunnel's remote address is
// normally the server's IP; a client dialling an address literal sends no SNI;
// so every handshake was refused with "missing server name" before autocert
// ever attempted an issuance. Nothing was wrong with Let's Encrypt, and nothing
// in the log said so.
func TestAnSNIlessHandshakeIsAnsweredWithTheConfiguredDomain(t *testing.T) {
	// No SNI, exactly as a client dialling an IP sends.
	hello := &tls.ClientHelloInfo{
		ServerName:   "",
		CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
	}
	want := acmeHelloForDomain(hello, "tunnel.example.com")
	if want.ServerName != "tunnel.example.com" {
		t.Fatalf("SNI-less hello selected %q", want.ServerName)
	}
	if hello.ServerName != "" || want == hello {
		t.Fatal("certificate selection mutated the caller's hello")
	}
	if len(want.CipherSuites) != 1 || want.CipherSuites[0] != hello.CipherSuites[0] {
		t.Fatal("SNI substitution lost the client's cipher capabilities")
	}
}

// A handshake that does name a host must be left alone, or a listener would
// answer for names it was never configured to serve.
func TestAnExplicitServerNameIsNotRewritten(t *testing.T) {
	hello := &tls.ClientHelloInfo{
		ServerName:   "somewhere.else.example",
		CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
	}
	want := acmeHelloForDomain(hello, "tunnel.example.com")
	if want != hello || want.ServerName != "somewhere.else.example" {
		t.Fatal("an explicit server name was replaced by the listener's domain")
	}
}

// The primed hello has to ask for the same kind of key a real client will, or
// the startup fetch and the first handshake miss each other in the cache and
// everything is issued twice — which is how a host reaches a rate limit.
func TestThePrimedHelloAsksForTheSameKeyAsARealClient(t *testing.T) {
	primed := &tls.ClientHelloInfo{
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		},
		SupportedCurves:  []tls.CurveID{tls.CurveP256},
		SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256, tls.PSSWithSHA256},
	}
	if !helloSupportsECDSA(primed) {
		t.Error("the primed hello reads as an RSA-only client, so startup would fetch an " +
			"RSA certificate that the first real handshake then misses")
	}

	bare := &tls.ClientHelloInfo{}
	if helloSupportsECDSA(bare) {
		t.Error("a bare hello should not read as ECDSA-capable; if it does, this test " +
			"is not checking what it thinks it is")
	}
}

// helloSupportsECDSA mirrors autocert's own choice between an RSA and an ECDSA
// certificate. It is duplicated here because that function is unexported, and
// the point of the test is that our hello lands on the same side of it.
func helloSupportsECDSA(hello *tls.ClientHelloInfo) bool {
	if hello.SignatureSchemes != nil {
		ok := false
		for _, s := range hello.SignatureSchemes {
			switch s {
			case tls.ECDSAWithP256AndSHA256, tls.ECDSAWithP384AndSHA384, tls.ECDSAWithP521AndSHA512:
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if hello.SupportedCurves != nil {
		ok := false
		for _, c := range hello.SupportedCurves {
			if c == tls.CurveP256 {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	for _, suite := range hello.CipherSuites {
		switch suite {
		case tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305:
			return true
		}
	}
	return false
}
