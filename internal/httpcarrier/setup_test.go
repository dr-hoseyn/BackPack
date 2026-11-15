package httpcarrier

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type setupRoundTripper func(*http.Request) (*http.Response, error)

func (f setupRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type setupResponseBody struct {
	io.Reader
	closed bool
}

func (b *setupResponseBody) Close() error { b.closed = true; return nil }

func TestCancelledCONNECTSetupCannotReturnSuccessfulStream(t *testing.T) {
	for _, lifetime := range []bool{false, true} {
		name := "dial context"
		if lifetime {
			name = "client lifetime"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dial, cancelDial := context.WithCancel(context.Background())
			defer cancelDial()
			c := &Client{ctx: ctx, endpoint: "localhost:443"}
			body := &setupResponseBody{Reader: strings.NewReader("")}
			var requestBody io.ReadCloser
			rt := setupRoundTripper(func(req *http.Request) (*http.Response, error) {
				requestBody = req.Body
				// A response can win a transport's select while cancellation
				// happens concurrently. Check ownership before handing it off.
				if lifetime {
					cancel()
				} else {
					cancelDial()
				}
				return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
			})
			s, err := c.connect(dial, "127.0.0.1:8080", rt)
			if s != nil {
				defer s.Close()
			}
			if s != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled setup returned stream %v, error %v", s != nil, err)
			}
			if !body.closed {
				t.Error("cancelled response body was retained")
			}
			if _, err := requestBody.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
				t.Errorf("cancelled upload pipe remained open: %v", err)
			}
		})
	}
}
