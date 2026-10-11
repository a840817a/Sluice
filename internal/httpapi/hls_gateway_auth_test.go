package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
)

// An HLS channel's playlists, segments and AES-128 key all carry the gateway
// auth header (ADR-0009), even when the key is fetched for decrypt-on-ingest.
func TestGatewayAuthHeaderReachesHLSKeyAndSegments(t *testing.T) {
	origin := startEncryptedOrigin(t, 3, true, nil)
	target, _ := url.Parse(origin.URL)
	rec := newHeaderRecorder()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind := "playlist"
		switch {
		case strings.HasSuffix(r.URL.Path, ".bin"):
			kind = "key"
		case strings.HasSuffix(r.URL.Path, ".ts"):
			kind = "segment"
		}
		rec.record(kind, r.Header)
		httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)

	cfg := config.Config{
		Server:   config.ServerConfig{BaseURL: "http://gw.test"},
		Store:    config.StoreConfig{DataDir: t.TempDir()},
		Worker:   config.WorkerConfig{FetchWorkers: 2, MaxRetriesStatic: 2},
		Window:   config.WindowConfig{Depth: time.Hour},
		Upstream: config.UpstreamConfig{AuthHeader: "X-Gateway-Auth", AuthValue: "gw-secret"},
	}
	mgr := newTestManager(t, cfg)
	if _, err := createChannel(mgr, context.Background(), "enc", ch.Config{
		SourceType: ch.SourceHLS,
		HLSKeyMode: ch.KeyModeDecrypt,
		MPDURL:     front.URL + "/master.m3u8",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("start channel: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop("enc") })

	waitForOriginRequests(t, rec, "playlist", "key", "segment")
	for _, kind := range []string{"playlist", "key", "segment"} {
		for i, h := range rec.get(kind) {
			if got := h.Get("X-Gateway-Auth"); got != "gw-secret" {
				t.Errorf("%s request %d: X-Gateway-Auth = %q, want gw-secret", kind, i, got)
			}
		}
	}
}
