package dashingest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/queue"
)

// staticAssetMPD names a finished asset of segCount two-second segments.
func staticAssetMPD(segCount int) string {
	dur := segCount * 2
	return fmt.Sprintf(`<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT%dS">
  <Period id="p0" duration="PT%dS">
    <AdaptationSet mimeType="video/mp4">
      <SegmentTemplate timescale="1000" duration="2000" startNumber="1"
                       initialization="v0_init.mp4" media="v0_$Number%%09d$.m4s"/>
      <Representation id="v0" bandwidth="1000000" codecs="avc1.64001f" width="640" height="360"/>
    </AdaptationSet>
  </Period>
</MPD>`, dur, dur)
}

// A static origin names every segment of a finished asset in a single
// discovery pass, and every one of them is required: unlike a live window,
// there is no later pass to pick up what was shed. When the asset is larger
// than the broker, the enumeration must wait for the pipeline rather than
// hand the broker more than it can hold — whose Push drops the overflow.
//
// Before backpressure this fails with ~cap segments enqueued and the rest
// counted in Dropped(), which is how a 3h36m asset became an 824-segment VOD.
func TestStaticIngestOutlastsBrokerCapacity(t *testing.T) {
	const segCount = 300
	const brokerCap = 32

	mux := http.NewServeMux()
	mux.HandleFunc("/manifest.mpd", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dash+xml")
		fmt.Fprint(w, staticAssetMPD(segCount))
	})
	origin := httptest.NewServer(mux)
	defer origin.Close()

	broker := queue.NewBroker(brokerCap)
	cfg := config.Config{
		Store:    config.StoreConfig{DataDir: t.TempDir()},
		Upstream: config.UpstreamConfig{MPDURL: origin.URL + "/manifest.mpd", PollInterval: 100 * time.Millisecond},
		Worker:   config.WorkerConfig{FetchWorkers: 1, MaxRetriesStatic: 1},
		Window:   config.WindowConfig{Depth: time.Hour},
	}
	w := NewWatcher(cfg, "bp", nil, index.NewChannelIndex(index.PublishAVBarrier), broker)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Stand in for the fetch pool: drain the broker so the producer can make
	// progress. Pop/Release must be paired or Idle never becomes true.
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
			task.Finish()
		}
	}()

	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run() = %v, want nil for a static origin", err)
	}
	// Run returned, so discovery is done; let the drainer finish the tail.
	for !broker.Idle() {
		time.Sleep(time.Millisecond)
	}
	close(stop)
	drained.Wait()

	mu.Lock()
	got := len(seen)
	mu.Unlock()

	if dropped := broker.Dropped(); dropped != 0 {
		t.Errorf("broker dropped %d tasks; a static asset has no segment to spare", dropped)
	}
	if got != segCount {
		t.Errorf("enqueued %d of %d segments", got, segCount)
	}
}
