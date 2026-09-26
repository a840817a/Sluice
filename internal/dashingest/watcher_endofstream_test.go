package dashingest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/queue"
)

// endOfStreamMPD renders a one-representation MPD of the given presentation
// type. The segments it names are never fetched here: the fetch pool is not
// running, so tasks pile up in the (unbounded) broker and the watcher's own
// loop is what the test observes.
func endOfStreamMPD(presentation string) string {
	extra, period := ` minimumUpdatePeriod="PT1S"`, ""
	if presentation == "static" {
		// A static MPD names the whole asset: the period duration is what
		// discovery counts segments from. Deliberately far longer than the live
		// window the dynamic variant offers, so the two are distinguishable.
		extra, period = ` mediaPresentationDuration="PT60S"`, ` duration="PT60S"`
	}
	return fmt.Sprintf(`<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="%s"%s>
  <Period id="p0"%s>
    <AdaptationSet mimeType="video/mp4">
      <SegmentTemplate timescale="1000" duration="2000" startNumber="1"
                       initialization="v0_init.mp4" media="v0_$Number%%09d$.m4s"/>
      <Representation id="v0" bandwidth="1000000" codecs="avc1.64001f" width="640" height="360"/>
    </AdaptationSet>
  </Period>
</MPD>`, presentation, extra, period)
}

// startFlippingOrigin serves a dynamic MPD until flip is set, then a static one.
func startFlippingOrigin(t *testing.T, flip *atomic.Bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest.mpd", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dash+xml")
		if flip.Load() {
			fmt.Fprint(w, endOfStreamMPD("static"))
			return
		}
		fmt.Fprint(w, endOfStreamMPD("dynamic"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newEndOfStreamWatcher(t *testing.T, mpdURL string) *Watcher {
	t.Helper()
	cfg := config.Config{
		Store:    config.StoreConfig{DataDir: t.TempDir()},
		Upstream: config.UpstreamConfig{MPDURL: mpdURL, PollInterval: 100 * time.Millisecond},
		Worker:   config.WorkerConfig{FetchWorkers: 1, MaxRetriesStatic: 1},
		Window:   config.WindowConfig{Depth: time.Hour},
	}
	return NewWatcher(cfg, "eos", nil, index.NewChannelIndex(index.PublishAVBarrier), queue.NewBroker(0))
}

// TestRunReturnsWhenOriginGoesStatic is the DASH half of "the origin ended".
//
// Returning nil is the only way a source reports that: the channel manager turns
// a clean Run return into the VOD transition, for both protocols. A watcher that
// keeps polling a finished stream leaves the channel live forever, which is what
// this asserts against — before the change the poll loop had no exit at all and
// this test fails by timing out with ctx.Err() instead of nil.
func TestRunReturnsWhenOriginGoesStatic(t *testing.T) {
	var flip atomic.Bool
	origin := startFlippingOrigin(t, &flip)
	w := newEndOfStreamWatcher(t, origin.URL+"/manifest.mpd")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Let the watcher settle into its dynamic poll loop before the flip, so the
	// test exercises the transition rather than the initially-static shortcut.
	time.Sleep(300 * time.Millisecond)
	if got := w.UpstreamType(); got != string(imdp.PresentationDynamic) {
		t.Fatalf("UpstreamType() = %q before flip, want dynamic", got)
	}
	flip.Store(true)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil after the origin went static", err)
		}
	case <-ctx.Done():
		t.Fatal("Run() did not return after the origin went static")
	}

	if got := w.UpstreamType(); got != string(imdp.PresentationStatic) {
		t.Errorf("UpstreamType() = %q after flip, want static", got)
	}
}

// TestRunEnqueuesFinalStaticTimeline pins the reason the final MPD is processed
// before Run returns: the static manifest names the whole asset, and returning
// as soon as the type flips would silently truncate the recording that becomes
// the VOD.
//
// The discriminator is the tail. Both manifests number from 1, but the live one
// only ever offers a window's worth (15 segments), while the static one declares
// a sixty-second asset — thirty. Segment 30 is nameable only by the final fetch,
// so asserting it reached the broker is what makes this test fail if the fetch
// is skipped; asserting on the low numbers both manifests share would pass
// against exactly the bug it is meant to catch.
func TestRunEnqueuesFinalStaticTimeline(t *testing.T) {
	var flip atomic.Bool
	origin := startFlippingOrigin(t, &flip)
	w := newEndOfStreamWatcher(t, origin.URL+"/manifest.mpd")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	time.Sleep(300 * time.Millisecond)
	live := highestQueuedSegment(w)
	flip.Store(true)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-ctx.Done():
		t.Fatal("Run() did not return after the origin went static")
	}

	if got := highestQueuedSegment(w); got <= live {
		t.Errorf("highest queued segment is %d, same as the live window's %d: "+
			"the final static timeline (30 segments) was never enqueued", got, live)
	}
}

// highestQueuedSegment drains the broker and reports the largest segment number
// it held. Draining is fine: no fetch pool is running, and every caller here
// only cares about how far the timeline reached.
func highestQueuedSegment(w *Watcher) uint64 {
	var max uint64
	for {
		task := w.broker.Pop()
		if task == nil {
			return max
		}
		if task.SegNo > max {
			max = task.SegNo
		}
	}
}
