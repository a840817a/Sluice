// Package license provides a transparent reverse-proxy for DRM license requests.
//
// The gateway rewrites the LA_URL inside the mspr:pro blob to point to itself,
// so players POST license challenges here. This handler forwards the request
// body verbatim to the upstream license server and pipes the response back.
package license

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
)

// Proxy forwards DRM license requests to the upstream license server.
type Proxy struct {
	upstreamURL     string
	extraHeaders    map[string]string // injected into every upstream license request
	client          *http.Client
	forwardClientIP bool
}

// NewProxy creates a license Proxy. headers are injected into every upstream request.
func NewProxy(upstreamURL string, headers map[string]string, forwardClientIP bool) *Proxy {
	return &Proxy{
		upstreamURL:     upstreamURL,
		extraHeaders:    headers,
		forwardClientIP: forwardClientIP,
		client:          &http.Client{Timeout: 15 * time.Second},
	}
}

// Handler returns an http.HandlerFunc that proxies license requests.
// The mount path is defined by gwurl.LicenseURL and registered in
// internal/httpapi/routes.go; do not restate it here, so the two cannot drift.
func (p *Proxy) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		channelID := chi.URLParam(r, "channelID")

		target, err := url.Parse(p.upstreamURL)
		if err != nil {
			slog.Error("license proxy: bad upstream URL", "err", err)
			http.Error(w, "proxy configuration error", http.StatusInternalServerError)
			return
		}

		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.String(), r.Body)
		if err != nil {
			http.Error(w, "failed to build upstream request", http.StatusInternalServerError)
			return
		}

		// Forward relevant headers from the client.
		for _, h := range []string{
			"Content-Type",
			"Content-Length",
			"Authorization",
			"X-Custom-Data",           // Widevine custom_data
			"X-Playready-Custom-Data", // PlayReady
		} {
			if v := r.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}

		// Inject upstream license headers.
		for k, v := range p.extraHeaders {
			req.Header.Set(k, v)
		}

		if p.forwardClientIP {
			ip := r.RemoteAddr
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				ip = xff + ", " + ip
			}
			req.Header.Set("X-Forwarded-For", ip)
		}

		resp, err := p.client.Do(req)
		if err != nil {
			slog.Error("license proxy: upstream error",
				"channel", channelID, "err", err)
			http.Error(w, "upstream license server error", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// Pass through response headers and status.
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)

		slog.Debug("license proxy: forwarded",
			"channel", channelID,
			"upstream_status", resp.StatusCode,
		)
	}
}
