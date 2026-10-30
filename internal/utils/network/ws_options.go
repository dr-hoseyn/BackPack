package network

import (
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
)

var errWSBinaryRequired = errors.New("websocket stream requires binary messages")

// WSDialOption configures one upgrade without changing the older call sites.
type WSDialOption interface{ applyWS(*wsDialSettings) }

type wsDialSettings struct {
	mux *WSMuxOptions
	tls *WSSClientOptions
}

// WSSClientOptions is owned by one transport configuration and shared by its
// control and pool dials. Reloading configuration creates a fresh CA snapshot.
type WSSClientOptions struct {
	CAFile            string
	ServerName        string
	AllowLegacyServer bool
	HTTP11Only        bool
	once              sync.Once
	roots             *x509.CertPool
	err               error
}

func (o *WSSClientOptions) applyWS(s *wsDialSettings) {
	if o != nil {
		s.tls = o
	}
}
func (o *WSMuxOptions) applyWS(s *wsDialSettings) { s.mux = o }

func (o *WSSClientOptions) rootCAs() (*x509.CertPool, error) {
	o.once.Do(func() {
		if o.CAFile == "" {
			return
		}
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			o.err = fmt.Errorf("wss: read trusted CA file: %w", err)
			return
		}
		o.roots, err = x509.SystemCertPool()
		if err != nil {
			o.roots = x509.NewCertPool()
		}
		if !o.roots.AppendCertsFromPEM(pem) {
			o.err = errors.New("wss: trusted CA file contains no certificates")
		}
	})
	return o.roots, o.err
}
