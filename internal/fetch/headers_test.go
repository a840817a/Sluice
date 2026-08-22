package fetch

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/a840817a/sluice/internal/queue"
)

// A channel's upstream credentials are rotated by swapping the provider while
// the fetch workers are running. If Fetcher ever captured the map instead of
// reading it per request, a token rotation would silently keep using the stale
// credential until the channel was restarted — which is exactly what the hot
// update exists to avoid.
func TestFetcherSendsRotatedHeaders(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("X-Token"))
		mu.Unlock()
		w.Write([]byte("data"))
	}))
	defer srv.Close()

	h := NewAtomicHeaders(map[string]string{"X-Token": "old"})
	f := NewFetcher(nil, h, 0, 0)

	if res := f.Fetch(t.Context(), &queue.SegmentTask{URL: srv.URL}, false); res.Err != nil {
		t.Fatalf("first fetch: %v", res.Err)
	}
	h.Store(map[string]string{"X-Token": "new"})
	if res := f.Fetch(t.Context(), &queue.SegmentTask{URL: srv.URL}, false); res.Err != nil {
		t.Fatalf("fetch after rotation: %v", res.Err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("origin saw %d requests, want 2", len(got))
	}
	if got[0] != "old" {
		t.Errorf("first request X-Token = %q, want %q", got[0], "old")
	}
	if got[1] != "new" {
		t.Errorf("request after rotation X-Token = %q, want %q", got[1], "new")
	}
}

func TestApplyHeadersToleratesNil(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// A nil interface and a nil concrete provider both mean "no headers"; every
	// consumer relies on this rather than branching at the call site.
	ApplyHeaders(req, nil)
	var typed *AtomicHeaders
	ApplyHeaders(req, typed)
	if n := len(req.Header); n != 0 {
		t.Errorf("nil providers set %d headers, want 0", n)
	}
}

// Snapshot must hand out a map nobody mutates, because every fetch worker and
// both watcher loops read it concurrently with no further synchronisation.
// Store copies its input so a caller that keeps writing to its own map cannot
// retroactively corrupt what readers see.
func TestStoreCopiesInput(t *testing.T) {
	src := map[string]string{"X-Token": "a"}
	h := NewAtomicHeaders(src)
	src["X-Token"] = "mutated-after-store"

	if got := h.Snapshot()["X-Token"]; got != "a" {
		t.Errorf("X-Token = %q, want %q — Store must copy its input", got, "a")
	}
}

func TestAtomicHeadersConcurrentReadWrite(t *testing.T) {
	h := NewAtomicHeaders(map[string]string{"X-Token": "0"})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 5000; i++ {
			h.Store(map[string]string{"X-Token": "rotated"})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 5000; i++ {
			for range h.Snapshot() {
			}
		}
	}()
	wg.Wait()
}
