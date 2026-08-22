package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
)

// headerRecorder captures the request headers an origin actually received,
// keyed by the kind of resource requested.
type headerRecorder struct {
	mu   sync.Mutex
	seen map[string][]http.Header
}

func newHeaderRecorder() *headerRecorder {
	return &headerRecorder{seen: make(map[string][]http.Header)}
}

func (h *headerRecorder) record(kind string, hdr http.Header) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen[kind] = append(h.seen[kind], hdr.Clone())
}

func (h *headerRecorder) get(kind string) []http.Header {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]http.Header(nil), h.seen[kind]...)
}

// startRecordingDASHOrigin is startClearDASHOrigin with request headers
// recorded, split into "manifest" and "segment" so the two paths can be
// asserted independently.
func startRecordingDASHOrigin(t *testing.T, rec *headerRecorder) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest.mpd", func(w http.ResponseWriter, r *http.Request) {
		rec.record("manifest", r.Header)
		w.Header().Set("Content-Type", "application/dash+xml")
		fmt.Fprint(w, clearDASHMPD())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		rec.record("segment", r.Header)
		name := strings.TrimPrefix(r.URL.Path, "/")
		switch {
		case strings.HasSuffix(name, "_init.mp4"):
			w.Write(fmp4Init(dashTimescale))
		case strings.HasSuffix(name, ".m4s"):
			var rep string
			var n int
			if _, err := fmt.Sscanf(name, "%2s_%09d.m4s", &rep, &n); err != nil {
				http.NotFound(w, r)
				return
			}
			w.Write(fmp4Segment(uint64(n-1) * dashSegDur))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// waitForOriginRequests blocks until the origin has received at least one
// request of every named kind, which is the precondition these tests actually
// need before asserting on the recorded headers.
func waitForOriginRequests(t *testing.T, rec *headerRecorder, kinds ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		missing := ""
		for _, k := range kinds {
			if len(rec.get(k)) == 0 {
				missing = k
				break
			}
		}
		if missing == "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Leave the assertion itself to the caller: it produces a better message.
	t.Logf("origin did not receive every expected request kind %v within 10s", kinds)
}

// channel.Config documents FetchHeaders as "injected into upstream MPD/segment
// requests". The segment half was wired through fetch.Fetcher; the manifest
// half was not — ingest.NewWatcher never received the headers, so a DASH
// channel behind an authenticating origin could download segments but not its
// own manifest. (HLS was unaffected: hlsingest.NewWatcher always took them.)
func TestDASHFetchHeadersReachManifestAndSegments(t *testing.T) {
	rec := newHeaderRecorder()
	origin := startRecordingDASHOrigin(t, rec)

	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: t.TempDir()},
		Worker: config.WorkerConfig{FetchWorkers: 2, MaxRetriesStatic: 2},
		Window: config.WindowConfig{Depth: time.Hour},
	}
	mgr := ch.NewManager(cfg)
	if _, err := mgr.Start(context.Background(), "hdr", ch.Config{
		MPDURL:  origin.URL + "/manifest.mpd",
		Enabled: true,
		FetchHeaders: []ch.Header{
			{Name: "X-Origin-Token", Value: "s3cret"},
			{Name: "X-Tenant", Value: "acme"},
		},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop("hdr") })

	waitForOriginRequests(t, rec, "manifest", "segment")

	manifestReqs := rec.get("manifest")
	if len(manifestReqs) == 0 {
		t.Fatal("origin never received a manifest request")
	}
	for i, h := range manifestReqs {
		if got := h.Get("X-Origin-Token"); got != "s3cret" {
			t.Errorf("manifest request %d: X-Origin-Token = %q, want s3cret", i, got)
		}
		if got := h.Get("X-Tenant"); got != "acme" {
			t.Errorf("manifest request %d: X-Tenant = %q, want acme", i, got)
		}
	}

	segReqs := rec.get("segment")
	if len(segReqs) == 0 {
		t.Fatal("origin never received a segment request")
	}
	for i, h := range segReqs {
		if got := h.Get("X-Origin-Token"); got != "s3cret" {
			t.Errorf("segment request %d: X-Origin-Token = %q, want s3cret", i, got)
		}
	}
}

// The gateway-wide Upstream.AuthHeader must still win over a same-named
// per-channel header, matching how hlsingest.Watcher.fetch orders them.
func TestGatewayAuthHeaderOverridesChannelHeader(t *testing.T) {
	rec := newHeaderRecorder()
	origin := startRecordingDASHOrigin(t, rec)

	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: t.TempDir()},
		Worker: config.WorkerConfig{FetchWorkers: 1, MaxRetriesStatic: 1},
		Window: config.WindowConfig{Depth: time.Hour},
		Upstream: config.UpstreamConfig{
			AuthHeader: "Authorization",
			AuthValue:  "Bearer gateway",
		},
	}
	mgr := ch.NewManager(cfg)
	if _, err := mgr.Start(context.Background(), "hdr2", ch.Config{
		MPDURL:       origin.URL + "/manifest.mpd",
		Enabled:      true,
		FetchHeaders: []ch.Header{{Name: "Authorization", Value: "Bearer channel"}},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop("hdr2") })

	waitForOriginRequests(t, rec, "manifest")

	reqs := rec.get("manifest")
	if len(reqs) == 0 {
		t.Fatal("origin never received a manifest request")
	}
	if got := reqs[0].Get("Authorization"); got != "Bearer gateway" {
		t.Errorf("Authorization = %q, want the gateway value to win", got)
	}
}
