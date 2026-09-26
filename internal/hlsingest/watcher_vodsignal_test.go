package hlsingest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/progress"
	"github.com/a840817a/sluice/internal/queue"
)

// What a finished playlist says about itself has to be readable while its
// segments are still being queued, not only once they all are.
//
// The channel's mode and its segment total are both derived from it, and the
// producer now paces itself against the broker — so for an asset larger than
// the queue it does not finish enqueueing until most of the download is done.
// Reading these afterwards left a VOD ingest reporting itself as live, with no
// total and therefore no progress bar, for nearly the whole run.
//
// The test pins that by never draining the broker: enqueueing cannot get past
// the queue's capacity, so anything observable here is observable mid-ingest.
func TestVODUpstreamIsKnownBeforeEnqueueFinishes(t *testing.T) {
	const segCount = 300
	const brokerCap = 32

	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1280000\nvod.m3u8\n"))
	})
	mux.HandleFunc("/vod.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MEDIA-SEQUENCE:0\n")
		for i := range segCount {
			fmt.Fprintf(&b, "#EXTINF:6.0,\ns%d.ts\n", i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		w.Write([]byte(b.String()))
	})
	origin := httptest.NewServer(mux)
	defer origin.Close()

	broker := queue.NewBroker(brokerCap)
	prog := progress.New()
	w := NewWatcher(config.Config{Store: config.StoreConfig{DataDir: t.TempDir()}},
		"vodsig", origin.URL+"/master.m3u8", nil, false, "",
		index.NewChannelIndex(index.PublishAVBarrier), broker)
	w.SetProgress(prog)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runReturned := make(chan struct{})
	go func() { defer close(runReturned); _ = w.Run(ctx) }()

	// Nothing drains the broker, so enqueueing is stuck at the queue's capacity
	// for the whole of this wait.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if w.UpstreamIsVOD() && prog.Snapshot().TotalKnown {
			break
		}
		if time.Now().After(deadline) {
			snap := prog.Snapshot()
			t.Fatalf("after 10s: UpstreamIsVOD()=%v TotalKnown=%v expected=%d — "+
				"a VOD ingest cannot report itself as one until it has finished queueing",
				w.UpstreamIsVOD(), snap.TotalKnown, snap.ExpectedSegments)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The claim is specifically that this is visible *mid-ingest*, so prove the
	// producer has not finished.
	select {
	case <-runReturned:
		t.Fatal("Run returned even though nothing drained the broker; " +
			"this test no longer proves the signal arrives mid-ingest")
	default:
	}

	if got := prog.Snapshot().ExpectedSegments; got != segCount {
		t.Errorf("ExpectedSegments = %d, want %d", got, segCount)
	}
	if depth := broker.Len(); depth > brokerCap {
		t.Errorf("queue depth %d exceeds capacity %d", depth, brokerCap)
	}

	cancel()
	<-runReturned
}
