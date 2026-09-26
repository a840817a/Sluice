package dashingest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/queue"
)

// multiRepMPD names two video representations and one audio representation,
// each with segCount segments — the shape of every real asset.
func multiRepMPD(segCount int) string {
	dur := segCount * 2
	return fmt.Sprintf(`<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT%dS">
  <Period id="p0" duration="PT%dS">
    <AdaptationSet mimeType="video/mp4">
      <SegmentTemplate timescale="1000" duration="2000" startNumber="1"
                       initialization="$RepresentationID$_init.mp4" media="$RepresentationID$_$Number%%09d$.m4s"/>
      <Representation id="v0" bandwidth="2000000" codecs="avc1.64001f" width="1280" height="720"/>
      <Representation id="v1" bandwidth="1000000" codecs="avc1.64001f" width="640" height="360"/>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4" lang="und">
      <SegmentTemplate timescale="1000" duration="2000" startNumber="1"
                       initialization="$RepresentationID$_init.mp4" media="$RepresentationID$_$Number%%09d$.m4s"/>
      <Representation id="a0" bandwidth="128000" codecs="mp4a.40.2"/>
    </AdaptationSet>
  </Period>
</MPD>`, dur, dur)
}

// Segments must be enqueued in presentation order across every representation,
// not one representation at a time.
//
// The broker is a priority queue, so while everything fits in it the pop order
// is by segment number regardless of how it was pushed. A bounded queue removes
// that safety net: whatever the producer has not pushed yet cannot be chosen,
// so feeding it representation by representation means the queue holds only the
// first representation and the rest of the asset waits. The A/V barrier then
// has no audio to align video against and publishes nothing, so a download that
// is visibly progressing stays unplayable to the end.
func TestTasksAreEnqueuedInPresentationOrder(t *testing.T) {
	const segCount = 300
	const brokerCap = 32

	mux := http.NewServeMux()
	mux.HandleFunc("/manifest.mpd", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dash+xml")
		fmt.Fprint(w, multiRepMPD(segCount))
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
	w := NewWatcher(cfg, "order", nil, index.NewChannelIndex(index.PublishAVBarrier), broker)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	type pop struct {
		rep   string
		segNo uint64
	}
	popped := make(chan pop, segCount*3)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
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
			popped <- pop{rep: task.RepID, segNo: task.SegNo}
			broker.Release()
		}
	}()

	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	for !broker.Idle() {
		time.Sleep(time.Millisecond)
	}
	close(stop)
	<-done
	close(popped)

	var order []pop
	for p := range popped {
		order = append(order, p)
	}
	if len(order) != segCount*3 {
		t.Fatalf("fetched %d tasks, want %d", len(order), segCount*3)
	}

	// Where does each representation first appear? A representation that only
	// starts once another has finished cannot be aligned against it.
	first := map[string]int{}
	for i, p := range order {
		if _, seen := first[p.rep]; !seen {
			first[p.rep] = i
		}
	}
	t.Logf("first appearance: v0=%d v1=%d a0=%d (of %d)",
		first["v0"], first["v1"], first["a0"], len(order))

	// Allow slack for the fetch loop racing the producer, but every
	// representation must be underway within the first few segments.
	const slack = 3 * 8
	for _, rep := range []string{"v0", "v1", "a0"} {
		if first[rep] > slack {
			t.Errorf("representation %s does not start until task %d of %d; "+
				"audio and video cannot be aligned, so nothing becomes playable",
				rep, first[rep], len(order))
		}
	}
}
