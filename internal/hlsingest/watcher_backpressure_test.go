package hlsingest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"context"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/queue"
)

// vodPlaylist renders a finished (EXT-X-ENDLIST) media playlist of segCount
// six-second segments.
func vodPlaylist(segCount int) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MEDIA-SEQUENCE:0\n")
	for i := range segCount {
		fmt.Fprintf(&b, "#EXTINF:6.0,\ns%d.ts\n", i)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// A VOD playlist is enumerated once — pollVariant returns the moment it sees
// EXT-X-ENDLIST — so every segment it advertises has exactly one chance to be
// queued. When the asset is larger than the broker, Push sheds the overflow and
// nothing ever comes back for it, which is the HLS form of the same defect the
// DASH static path had.
func TestVODPlaylistOutlastsBrokerCapacity(t *testing.T) {
	const segCount = 300
	const brokerCap = 32

	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1280000\nvod.m3u8\n"))
	})
	mux.HandleFunc("/vod.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(vodPlaylist(segCount)))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	broker := queue.NewBroker(brokerCap)
	w := NewWatcher(config.Config{Store: config.StoreConfig{DataDir: t.TempDir()}},
		"bp", srv.URL+"/master.m3u8", nil, false, "",
		index.NewChannelIndex(), broker)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Stand in for the fetch pool. Pop/Release must be paired or Idle never
	// becomes true.
	var mu sync.Mutex
	seen := map[uint64]bool{}
	stop := make(chan struct{})
	var drained sync.WaitGroup
	drained.Add(1)
	go func() {
		defer drained.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			task := broker.Pop()
			if task == nil {
				time.Sleep(time.Millisecond)
				continue
			}
			mu.Lock()
			seen[task.SegNo] = true
			mu.Unlock()
			broker.Release()
		}
	}()

	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run() = %v, want nil once every variant has ended", err)
	}
	for !broker.Idle() {
		time.Sleep(time.Millisecond)
	}
	close(stop)
	drained.Wait()

	mu.Lock()
	got := len(seen)
	mu.Unlock()

	if dropped := broker.Dropped(); dropped != 0 {
		t.Errorf("broker dropped %d tasks; a finished playlist has no segment to spare", dropped)
	}
	if got != segCount {
		t.Errorf("enqueued %d of %d segments", got, segCount)
	}
}
