// Package updatehealth verifies a known HTTP response through a tunnel's
// forwarded entry before and after an update. It never guesses an application
// protocol or sends test bytes to an arbitrary backend.
package updatehealth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
)

const maxBody = 64 << 10

// Check names an explicitly provisioned health endpoint. ExpectedBody is exact,
// including any trailing newline. The URL must address a local forwarded port;
// the management layer checks that the named tunnel owns that mapping.
type Check struct {
	Tunnel       string `json:"tunnel"`
	URL          string `json:"url"`
	ExpectedBody string `json:"expected_body"`
}

type Policy struct {
	GraceSeconds int     `json:"grace_seconds"`
	Checks       []Check `json:"checks"`
}

func (p Policy) Grace() time.Duration {
	if p.GraceSeconds == 0 {
		return 45 * time.Second
	}
	return time.Duration(p.GraceSeconds) * time.Second
}

// Load treats an absent policy as no configured payload checks. Malformed or
// oversized policies fail closed before an updater touches the installed binary.
func Load(path string) (Policy, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Policy{}, nil
	}
	if err != nil {
		return Policy{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (256<<10)+1))
	if err != nil || len(b) > 256<<10 {
		return Policy{}, errors.New("update health policy could not be read within 256 KiB")
	}
	var p Policy
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return Policy{}, errors.New("invalid update health JSON or unknown field")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Policy{}, errors.New("extra content after update health policy")
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

func (p Policy) Validate() error {
	if p.GraceSeconds < 0 || p.GraceSeconds > 120 {
		return errors.New("update health grace_seconds must be 0..120 (0 uses 45 seconds)")
	}
	if len(p.Checks) > 32 {
		return errors.New("at most 32 update health checks are allowed")
	}
	seen := make(map[string]bool)
	for _, c := range p.Checks {
		if c.Tunnel == "" || seen[c.Tunnel] {
			return errors.New("update health checks need unique, nonempty tunnel names")
		}
		seen[c.Tunnel] = true
		if _, err := Endpoint(c.URL); err != nil {
			return err
		}
		if len(c.ExpectedBody) == 0 || len(c.ExpectedBody) > maxBody {
			return errors.New("update health expected_body must contain 1..65536 bytes")
		}
	}
	return nil
}

// Endpoint refuses DNS, proxies and external hosts: a request must enter a
// local forwarding listener, rather than succeeding via the host's Internet
// route while the tunnel is broken.
func Endpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("update health URL must be HTTP(S), without credentials or fragments")
	}
	ip := net.ParseIP(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if ip == nil || !ip.IsLoopback() || err != nil || port < 1 || port > 65535 {
		return nil, errors.New("update health URL must name a literal loopback address and an explicit port")
	}
	return u, nil
}

// Probe uses a fresh connection and nonce for every attempt, ignores proxy
// environment variables, refuses redirects and bounds headers, body and time.
// A successful TCP connect or an HTTP 200 alone cannot pass it.
func Probe(ctx context.Context, c Check) error {
	u, err := Endpoint(c.URL)
	if err != nil {
		return err
	}
	if len(c.ExpectedBody) == 0 || len(c.ExpectedBody) > maxBody {
		return errors.New("invalid expected response size")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errors.New("could not generate health request nonce")
	}
	q := u.Query()
	q.Set("_backpack_health", hex.EncodeToString(nonce[:]))
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return errors.New("could not construct health request")
	}
	req.Header.Set("Cache-Control", "no-store, no-cache")
	req.Header.Set("Pragma", "no-cache")
	transport := &http.Transport{
		Proxy: nil, DisableKeepAlives: true, DisableCompression: true,
		DialContext:            (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxResponseHeaderBytes: 16 << 10,
		TLSHandshakeTimeout:    5 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("forwarded health request failed or timed out")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("forwarded health endpoint returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(len(c.ExpectedBody)+1)))
	if err != nil {
		return errors.New("forwarded health response was incomplete")
	}
	if !bytes.Equal(body, []byte(c.ExpectedBody)) {
		return errors.New("forwarded health response did not match expected_body")
	}
	return nil
}

type Failure struct {
	Tunnel string
	Detail string
}

// Verify checks independently, with at most four concurrent requests and one
// shared deadline. With retry=true, a startup failure may recover during the
// grace period. Every configured check must pass; one healthy tunnel cannot
// hide a broken one. Results retain policy order.
func Verify(ctx context.Context, checks []Check, retry bool) []Failure {
	errs := make([]error, len(checks))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, c := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					errs[i] = errors.New("health verification deadline exceeded")
					return
				}
				err := Probe(ctx, c)
				<-sem
				errs[i] = err
				if err == nil || !retry {
					return
				}
				timer := time.NewTimer(500 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}
	wg.Wait()
	var bad []Failure
	for i, err := range errs {
		if err != nil {
			bad = append(bad, Failure{Tunnel: checks[i].Tunnel, Detail: err.Error()})
		}
	}
	return bad
}
