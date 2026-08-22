package channel

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocfg "github.com/a840817a/sluice/internal/config"
)

// tsSegmentBytes returns a structurally valid MPEG-TS segment: whole 188-byte
// packets starting with the 0x47 sync byte, which is what validate.ValidateTS
// checks before the processor will store anything.
func tsSegmentBytes(n int) []byte {
	const packets = 3
	out := make([]byte, 0, packets*188)
	for p := 0; p < packets; p++ {
		pkt := make([]byte, 188)
		pkt[0] = 0x47
		pkt[1] = byte(n)
		pkt[2] = byte(p)
		for i := 3; i < 188; i++ {
			pkt[i] = byte((n*31 + p*7 + i) % 251)
		}
		out = append(out, pkt...)
	}
	return out
}

// startSlowHLSOrigin serves an HLS source whose segments each take segDelay to
// respond. The delay is the point: it guarantees that at any instant several
// fetches are in flight and several results are queued for the processor, so a
// Stop that does not drain leaves work running behind it.
func startSlowHLSOrigin(t *testing.T, segCount int, segDelay time.Duration, started *int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n"+
			"#EXT-X-VERSION:3\n"+
			"#EXT-X-INDEPENDENT-SEGMENTS\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS=\"avc1.4d401e\"\n"+
			"media/v0.m3u8\n")
	})

	mux.HandleFunc("/media/v0.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
		for i := 0; i < segCount; i++ {
			fmt.Fprintf(&b, "#EXTINF:4.000,\nseg%d.ts\n", i)
		}
		fmt.Fprint(w, b.String())
	})

	mux.HandleFunc("/media/", func(w http.ResponseWriter, r *http.Request) {
		var n int
		if _, err := fmt.Sscanf(r.URL.Path, "/media/seg%d.ts", &n); err != nil {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(started, 1)
		select {
		case <-time.After(segDelay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(tsSegmentBytes(n))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// filesUnder returns every regular file below root, relative and sorted, so two
// snapshots can be compared directly.
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

// TestStopLeavesNoWritesBehind is a cheap upper-bound guard: the data dir is
// snapshotted the instant Stop returns and again well after every in-flight
// fetch could have completed, and must not change.
//
// Be honest about its strength: segment fetches are context-aware, so a cancel
// aborts them long before they can write, and Processor.handle is fast. It
// passes both with and without a draining Stop. It is kept because it costs
// under a second and would catch a future regression that makes the post-cancel
// write window wide — but the real contract is pinned by
// TestStopCancelsInFlightKeyFetch below.
func TestStopLeavesNoWritesBehind(t *testing.T) {
	const segDelay = 200 * time.Millisecond

	var segStarted int32
	origin := startSlowHLSOrigin(t, 40, segDelay, &segStarted)

	cfg := gocfg.Config{
		Server: gocfg.ServerConfig{BaseURL: "http://gw.test"},
		Store:  gocfg.StoreConfig{DataDir: t.TempDir()},
		Worker: gocfg.WorkerConfig{FetchWorkers: 4, MaxRetriesStatic: 1},
		Window: gocfg.WindowConfig{Depth: time.Hour},
	}
	m := NewManager(cfg)
	t.Cleanup(m.StopAll)

	if _, err := m.Start(context.Background(), "ch1", Config{
		SourceType: SourceHLS,
		MPDURL:     origin.URL + "/master.m3u8",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Wait until ingest is genuinely mid-flight: segments already on disk and
	// more fetches still running. Stopping before this proves nothing.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if len(filesUnder(t, cfg.Store.DataDir)) > 0 && atomic.LoadInt32(&segStarted) >= 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ingest never started: %d segment requests, %d files",
				atomic.LoadInt32(&segStarted), len(filesUnder(t, cfg.Store.DataDir)))
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := m.Stop("ch1"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	after := filesUnder(t, cfg.Store.DataDir)

	// Generous margin: longer than a full segment fetch plus its processing.
	time.Sleep(4 * segDelay)
	settled := filesUnder(t, cfg.Store.DataDir)

	if len(settled) != len(after) {
		t.Errorf("data dir kept changing after Stop returned: %d files at Stop, %d files %v later\n"+
			"at Stop:  %v\nsettled:  %v\n"+
			"Stop must join the fetch pool and processor before returning, "+
			"or a restarted channel races the old one on the same files",
			len(after), len(settled), 4*segDelay, after, settled)
	}
}

// startHangingKeyHLSOrigin serves an AES-128 HLS source whose key endpoint never
// answers. The key handler blocks until its request context is cancelled and
// reports which happened, which is what lets the test distinguish "the gateway
// gave up because the channel stopped" from "the gateway is still waiting".
func startHangingKeyHLSOrigin(t *testing.T, segCount int, keyHit chan<- struct{}, keyCancelled chan<- struct{}) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var keyOnce sync.Once

	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n"+
			"#EXT-X-VERSION:3\n"+
			"#EXT-X-INDEPENDENT-SEGMENTS\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS=\"avc1.4d401e\"\n"+
			"media/v0.m3u8\n")
	})

	mux.HandleFunc("/media/v0.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
		b.WriteString("#EXT-X-KEY:METHOD=AES-128,URI=\"../key\",IV=0x00000000000000000000000000000000\n")
		for i := 0; i < segCount; i++ {
			fmt.Fprintf(&b, "#EXTINF:4.000,\nseg%d.ts\n", i)
		}
		fmt.Fprint(w, b.String())
	})

	// The key never arrives. Report the first request, then wait to be cancelled.
	mux.HandleFunc("/key", func(w http.ResponseWriter, r *http.Request) {
		keyOnce.Do(func() { close(keyHit) })
		<-r.Context().Done()
		select {
		case keyCancelled <- struct{}{}:
		default:
		}
	})

	mux.HandleFunc("/media/", func(w http.ResponseWriter, r *http.Request) {
		var n int
		if _, err := fmt.Sscanf(r.URL.Path, "/media/seg%d.ts", &n); err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		// Ciphertext is only length-checked (validate.ValidateTSEncrypted), so
		// any multiple of the AES block size stands in for a real segment.
		w.Write(make([]byte, 576))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestStopCancelsInFlightKeyFetch pins the drain contract at the one place where
// it is deterministically observable.
//
// A passthrough AES-128 channel captures the content key during ingest so
// playback survives the upstream dying. That fetch runs off the ingest path with
// its own 15s timeout. If it is rooted at context.Background() rather than the
// channel's context, Stop returns while it is still running: the goroutine, its
// HTTP connection and its write into the key cache outlive the channel by up to
// keyFetchTimeout, and an admin restart brings up a replacement channel whose
// key cache is being written by its predecessor.
//
// Unlike a segment fetch this is not a narrow race — the leak lasts 15 seconds
// and is fully reproducible, which is what makes it the right test to drive the
// fix. The bound below is far under that timeout, so a pass cannot be the
// timeout firing on its own.
func TestStopCancelsInFlightKeyFetch(t *testing.T) {
	keyHit := make(chan struct{})
	keyCancelled := make(chan struct{}, 1)
	origin := startHangingKeyHLSOrigin(t, 10, keyHit, keyCancelled)

	cfg := gocfg.Config{
		Server: gocfg.ServerConfig{BaseURL: "http://gw.test"},
		Store:  gocfg.StoreConfig{DataDir: t.TempDir()},
		Worker: gocfg.WorkerConfig{FetchWorkers: 2, MaxRetriesStatic: 1},
		Window: gocfg.WindowConfig{Depth: time.Hour},
	}
	m := NewManager(cfg)
	t.Cleanup(m.StopAll)

	if _, err := m.Start(context.Background(), "ch1", Config{
		SourceType: SourceHLS,
		MPDURL:     origin.URL + "/master.m3u8",
		HLSKeyMode: KeyModePassthrough,
		Enabled:    true,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	select {
	case <-keyHit:
	case <-time.After(15 * time.Second):
		t.Fatal("the gateway never requested the content key; the test never reached the path it covers")
	}

	if err := m.Stop("ch1"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	select {
	case <-keyCancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("the key fetch was still running 3s after Stop returned: it is rooted at " +
			"context.Background(), so it outlives the channel by up to keyFetchTimeout (15s). " +
			"Stop must cancel and join the ingest goroutines before returning.")
	}
}
