package license

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// licenseServerRecorder stands in for the DRM license server and records what the
// proxy actually sent it.
type licenseServerRecorder struct {
	mu     sync.Mutex
	gotReq *http.Request
	gotBdy string

	status  int
	body    string
	headers map[string]string
}

func newLicenseServer(t *testing.T, rec *licenseServerRecorder) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.gotReq = r.Clone(r.Context())
		rec.gotBdy = string(b)
		rec.mu.Unlock()

		for k, v := range rec.headers {
			w.Header().Set(k, v)
		}
		if rec.status != 0 {
			w.WriteHeader(rec.status)
		}
		_, _ = io.WriteString(w, rec.body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// serve runs one request through the proxy and returns the recorder.
func serve(t *testing.T, p *Proxy, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	p.Handler()(w, req)
	return w
}

// A license challenge is a POST. Anything else is a client that has misread the
// protocol, and forwarding it would hand an arbitrary method to the license
// server.
func TestProxyRejectsNonPOST(t *testing.T) {
	rec := &licenseServerRecorder{body: "license"}
	up := newLicenseServer(t, rec)
	p := NewProxy(up.URL, nil, false)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodHead} {
		w := serve(t, p, httptest.NewRequest(method, "/license", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want %d", method, w.Code, http.StatusMethodNotAllowed)
		}
		rec.mu.Lock()
		reached := rec.gotReq != nil
		rec.mu.Unlock()
		if reached {
			t.Errorf("%s reached the license server", method)
		}
	}
}

// The challenge is opaque bytes; the proxy must not reinterpret it.
func TestProxyForwardsBodyVerbatim(t *testing.T) {
	rec := &licenseServerRecorder{body: "ok"}
	up := newLicenseServer(t, rec)
	p := NewProxy(up.URL, nil, false)

	challenge := "\x00\x01binary challenge\xff"
	req := httptest.NewRequest(http.MethodPost, "/license", strings.NewReader(challenge))
	serve(t, p, req)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.gotBdy != challenge {
		t.Errorf("request body seen by the license server = %q, want %q", rec.gotBdy, challenge)
	}
	if rec.gotReq.Method != http.MethodPost {
		t.Errorf("method seen by the license server = %s, want POST", rec.gotReq.Method)
	}
}

// The forwarded set is an allowlist, and that is a security boundary: it decides
// what of the viewer's request reaches a third-party license server. Both halves
// matter — what crosses, and what does not.
func TestProxyForwardsOnlyAllowlistedHeaders(t *testing.T) {
	rec := &licenseServerRecorder{body: "ok"}
	up := newLicenseServer(t, rec)
	p := NewProxy(up.URL, nil, false)

	req := httptest.NewRequest(http.MethodPost, "/license", strings.NewReader("c"))
	forwarded := map[string]string{
		"Content-Type":            "application/octet-stream",
		"Authorization":           "Bearer token123",
		"X-Custom-Data":           "widevine-custom",
		"X-Playready-Custom-Data": "playready-custom",
	}
	for k, v := range forwarded {
		req.Header.Set(k, v)
	}
	// Client state that has no business reaching a third-party license server.
	dropped := map[string]string{
		"Cookie":     "session=secret",
		"Referer":    "https://internal.example/player",
		"User-Agent": "SomePlayer/1.0",
		"X-Api-Key":  "leak-me",
	}
	for k, v := range dropped {
		req.Header.Set(k, v)
	}
	serve(t, p, req)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for k, want := range forwarded {
		if got := rec.gotReq.Header.Get(k); got != want {
			t.Errorf("license server saw %s = %q, want %q", k, got, want)
		}
	}
	for k, clientValue := range dropped {
		// Not "is the header absent": Go's client supplies its own User-Agent
		// on the outgoing request. What must not happen is the client's value
		// crossing over.
		if got := rec.gotReq.Header.Get(k); got == clientValue {
			t.Errorf("the client's %s (%q) reached the license server; it is not on the allowlist", k, got)
		}
	}
}

// Per-channel credentials for the license server itself.
func TestProxyInjectsConfiguredHeaders(t *testing.T) {
	rec := &licenseServerRecorder{body: "ok"}
	up := newLicenseServer(t, rec)
	p := NewProxy(up.URL, map[string]string{
		"X-License-Token": "channel-secret",
		"Authorization":   "Bearer gateway-owned",
	}, false)

	req := httptest.NewRequest(http.MethodPost, "/license", strings.NewReader("c"))
	req.Header.Set("Authorization", "Bearer client-supplied")
	serve(t, p, req)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if got := rec.gotReq.Header.Get("X-License-Token"); got != "channel-secret" {
		t.Errorf("X-License-Token = %q, want %q", got, "channel-secret")
	}
	// The configured value wins: the gateway's own credential must not be
	// displaceable by whatever the player chose to send.
	if got := rec.gotReq.Header.Get("Authorization"); got != "Bearer gateway-owned" {
		t.Errorf("Authorization = %q, want the configured value to override the client's", got)
	}
}

func TestProxyClientIPForwarding(t *testing.T) {
	tests := []struct {
		name       string
		forward    bool
		remoteAddr string
		existing   string
		want       string
	}{
		{
			name: "disabled sends nothing", forward: false,
			remoteAddr: "203.0.113.7:54321", want: "",
		},
		{
			// X-Forwarded-For carries addresses, not address:port. A license
			// server matching an allowlist against "203.0.113.7:54321" matches
			// nothing.
			name: "enabled sends the client address without its port", forward: true,
			remoteAddr: "203.0.113.7:54321", want: "203.0.113.7",
		},
		{
			name: "enabled appends to an existing chain", forward: true,
			remoteAddr: "203.0.113.7:54321", existing: "198.51.100.1",
			want: "198.51.100.1, 203.0.113.7",
		},
		{
			name: "IPv6 client address", forward: true,
			remoteAddr: "[2001:db8::1]:54321", want: "2001:db8::1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &licenseServerRecorder{body: "ok"}
			up := newLicenseServer(t, rec)
			p := NewProxy(up.URL, nil, tc.forward)

			req := httptest.NewRequest(http.MethodPost, "/license", strings.NewReader("c"))
			req.RemoteAddr = tc.remoteAddr
			if tc.existing != "" {
				req.Header.Set("X-Forwarded-For", tc.existing)
			}
			serve(t, p, req)

			rec.mu.Lock()
			defer rec.mu.Unlock()
			if got := rec.gotReq.Header.Get("X-Forwarded-For"); got != tc.want {
				t.Errorf("X-Forwarded-For = %q, want %q", got, tc.want)
			}
		})
	}
}

// The license server's answer is the product: status, headers and body all
// belong to the player, including a refusal.
func TestProxyPassesLicenseServerResponseThrough(t *testing.T) {
	rec := &licenseServerRecorder{
		status:  http.StatusForbidden,
		body:    "entitlement denied",
		headers: map[string]string{"Content-Type": "application/octet-stream", "X-License-Reason": "no-subscription"},
	}
	up := newLicenseServer(t, rec)
	p := NewProxy(up.URL, nil, false)

	w := serve(t, p, httptest.NewRequest(http.MethodPost, "/license", strings.NewReader("c")))

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if got := w.Body.String(); got != "entitlement denied" {
		t.Errorf("body = %q, want %q", got, "entitlement denied")
	}
	if got := w.Header().Get("X-License-Reason"); got != "no-subscription" {
		t.Errorf("X-License-Reason = %q, want it passed through", got)
	}
}

// An unreachable license server is the remote side's failure, not the
// gateway's, and the player must not be handed the internal error text.
func TestProxyLicenseServerFailureIsBadGateway(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	downURL := down.URL
	down.Close() // nothing is listening now

	p := NewProxy(downURL, nil, false)
	w := serve(t, p, httptest.NewRequest(http.MethodPost, "/license", strings.NewReader("c")))

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
	if strings.Contains(w.Body.String(), downURL) {
		t.Errorf("response body leaks the license server URL: %q", w.Body.String())
	}
}

// A channel configured with an unusable license URL is a configuration error,
// and saying so is more useful than a bad gateway.
func TestProxyBadLicenseServerURLIsConfigurationError(t *testing.T) {
	p := NewProxy("://not-a-url", nil, false)
	w := serve(t, p, httptest.NewRequest(http.MethodPost, "/license", strings.NewReader("c")))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

// A license server that accepts the connection and then stalls must not hold the
// player's request open indefinitely. The timeout lives on the Proxy's own
// client; this test replaces it rather than waiting out the real 15 seconds,
// which is available here because the test sits in the same package — the client
// needs no injection seam for this.
func TestProxyTimesOutASlowLicenseServer(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // stall until the test is done
	}))
	t.Cleanup(func() { close(release); up.Close() })

	p := NewProxy(up.URL, nil, false)
	p.client = &http.Client{Timeout: 50 * time.Millisecond}

	done := make(chan int, 1)
	go func() {
		w := serve(t, p, httptest.NewRequest(http.MethodPost, "/license", strings.NewReader("c")))
		done <- w.Code
	}()

	select {
	case code := <-done:
		if code != http.StatusBadGateway {
			t.Errorf("status = %d, want %d", code, http.StatusBadGateway)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy did not give up on a stalled license server")
	}
}
