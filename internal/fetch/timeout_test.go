package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/queue"
)

// slowOrigin dribbles a segment out over roughly total, flushing as it goes, so
// the transfer is genuinely progressing rather than stalled. That distinction is
// the point: http.Client.Timeout bounds the whole request including the body
// read, so it cuts off a healthy-but-slow download exactly as it would a hung
// one — which is what a bandwidth-limited deployment runs into.
func slowOrigin(t *testing.T, chunks int, perChunk time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			if _, err := w.Write(make([]byte, 1024)); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-time.After(perChunk):
			case <-r.Context().Done():
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fetchOnce(f *Fetcher, url string) Result {
	return f.Fetch(context.Background(), &queue.SegmentTask{URL: url, Format: queue.FormatTS}, false)
}

// A download slower than the configured timeout must fail, and the failure must
// be the timeout rather than something else.
func TestSegmentTimeoutCutsOffSlowDownload(t *testing.T) {
	origin := slowOrigin(t, 20, 40*time.Millisecond) // ~800ms of transfer

	f := NewFetcher(nil, nil, 0, 200*time.Millisecond)
	res := fetchOnce(f, origin.URL+"/seg0.ts")

	if res.Err == nil {
		t.Fatalf("expected a timeout, got %d bytes", len(res.Data))
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) &&
		!strings.Contains(res.Err.Error(), "Client.Timeout") &&
		!strings.Contains(res.Err.Error(), "deadline exceeded") {
		t.Errorf("error is not a timeout: %v", res.Err)
	}
}

// The same download must succeed once the timeout is raised — this is the half
// that proves the knob is actually wired, not just accepted.
func TestSegmentTimeoutIsHonoured(t *testing.T) {
	origin := slowOrigin(t, 20, 40*time.Millisecond) // ~800ms of transfer

	f := NewFetcher(nil, nil, 0, 10*time.Second)
	res := fetchOnce(f, origin.URL+"/seg0.ts")

	if res.Err != nil {
		t.Fatalf("download failed with a generous timeout: %v", res.Err)
	}
	if len(res.Data) != 20*1024 {
		t.Errorf("got %d bytes, want %d", len(res.Data), 20*1024)
	}
}

// Zero means "use the default", so existing callers that never set it keep the
// behaviour they had.
func TestSegmentTimeoutZeroUsesDefault(t *testing.T) {
	f := NewFetcher(nil, nil, 0, 0)
	if f.client.Timeout != DefaultSegmentTimeout {
		t.Errorf("client timeout = %v, want the default %v", f.client.Timeout, DefaultSegmentTimeout)
	}
}
