package hlsingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/queue"
)

// TestEndListProgressReportsWhoHasEnded covers the operator's question when a
// channel will not settle into VOD: which playlist is still open.
//
// One variant carries EXT-X-ENDLIST and the other never will, which is exactly
// the case that hangs a channel in live indefinitely — pollVariant retries a
// failing or endless playlist forever, so nothing else in the system says
// anything about it. Without the counters this reports 0/0 and the situation
// stays invisible.
func TestEndListProgressReportsWhoHasEnded(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#EXTM3U\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=1280000\nended.m3u8\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=640000\nlive.m3u8\n"))
	})
	mux.HandleFunc("/ended.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-PLAYLIST-TYPE:VOD\n" +
			"#EXTINF:6.0,\ne0.ts\n#EXT-X-ENDLIST\n"))
	})
	mux.HandleFunc("/live.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.0,\nl0.ts\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	w := NewWatcher(config.Config{Store: config.StoreConfig{DataDir: t.TempDir()}},
		"ch1", srv.URL+"/master.m3u8", nil, false, "",
		index.NewChannelIndex(), queue.NewBroker(64))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		ended, total := w.EndListProgress()
		if ended == 1 && total == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("EndListProgress() = (%d, %d), want (1, 2)", ended, total)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// And the live variant genuinely has not ended: Run must still be going.
	cancel()
}

// TestEndListProgressBeforeDiscovery pins the nil-status case the admin API
// depends on: nothing to report reads as (0, 0), never as "0 of 0 ended", so
// buildStatus can leave the field out entirely.
func TestEndListProgressBeforeDiscovery(t *testing.T) {
	w := newTestWatcher()
	if ended, total := w.EndListProgress(); ended != 0 || total != 0 {
		t.Errorf("EndListProgress() = (%d, %d) before Run, want (0, 0)", ended, total)
	}
}
