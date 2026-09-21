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

// A retry of a finished rendition's segment must not lose to admission from a
// concurrent producer. The interleaving is forced rather than raced for: each
// watcher's Run returns only after its finished playlist is fully enumerated,
// so the test decides exactly when each producer admits and when the retry
// arrives. With capacity 1 the sequence is
//
//	rendition A admits a0 -> fetch pool pops a0 (in flight, queue empty)
//	rendition B admits b0 -> queue full
//	a0 fails and is requeued while the queue is full
//
// Requeue must put a0 back without shedding it or b0; a broker that runs the
// retry through ordinary capacity-bounded admission drops one of them.
func TestFinishedRenditionsSurviveConcurrentAdmissionAndRetries(t *testing.T) {
	mux := http.NewServeMux()
	for _, r := range []string{"a", "b"} {
		mux.HandleFunc("/"+r+"/master.m3u8", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=100000\nmedia.m3u8\n")
		})
		mux.HandleFunc("/"+r+"/media.m3u8", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, vodPlaylist(1))
		})
	}
	origin := httptest.NewServer(mux)
	defer origin.Close()

	// Failure guard only; nothing below waits on time.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	broker := queue.NewBroker(1)
	admit := func(channel string) {
		t.Helper()
		w := NewWatcher(config.Config{Store: config.StoreConfig{DataDir: t.TempDir()}},
			channel, origin.URL+"/"+channel+"/master.m3u8", nil, false, "",
			index.NewChannelIndex(), broker)
		if err := w.Run(ctx); err != nil {
			t.Fatalf("watcher %s: Run() = %v, want nil for a finished playlist", channel, err)
		}
	}

	admit("a")
	inFlight := broker.Pop()
	if inFlight == nil || inFlight.ChannelID != "a" {
		t.Fatalf("Pop() = %v, want rendition a's only segment", inFlight)
	}

	admit("b")
	if n := broker.Len(); n != 1 {
		t.Fatalf("queue holds %d tasks after b's admission, want 1 (full)", n)
	}

	// Zero backoff: the retry is immediately poppable again.
	if !broker.Requeue(inFlight, 3, 0) {
		t.Fatal("finite task could not be retried")
	}
	broker.Release()
	if d := broker.Dropped(); d != 0 {
		t.Fatalf("broker dropped %d tasks when a retry met a full queue; finished renditions have none to spare", d)
	}

	stored := map[string]int{}
	for !broker.Idle() {
		task := broker.Pop()
		if task == nil {
			t.Fatalf("broker not idle but nothing is ready: len=%d stored=%v", broker.Len(), stored)
		}
		stored[task.ChannelID]++
		broker.Release()
	}
	if stored["a"] != 1 || stored["b"] != 1 || broker.Dropped() != 0 {
		t.Fatalf("stored=%v dropped=%d, want one segment from each rendition and no drops", stored, broker.Dropped())
	}
}
