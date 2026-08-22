package dashingest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/progress"
	"github.com/a840817a/sluice/internal/queue"
)

// newProgressWatcher builds a Watcher wired to fresh counters, for exercising the
// glue between discovery and the progress totals the admin UI divides by.
func newProgressWatcher(t *testing.T) (*Watcher, *progress.Counters) {
	t.Helper()
	cfg := config.Config{
		Store:  config.StoreConfig{DataDir: t.TempDir()},
		Worker: config.WorkerConfig{FetchWorkers: 1, MaxRetriesStatic: 1},
		Window: config.WindowConfig{Depth: time.Hour},
	}
	ci := index.NewChannelIndex()
	broker := queue.NewBroker(0) // unbounded: nothing may be dropped during the test
	w := NewWatcher(cfg, "ch1", nil, ci, broker)
	prog := progress.New()
	w.SetProgress(prog)
	return w, prog
}

// TestProcessMPDPublishesStaticTotal covers the wiring between discovery and the
// counters: the number the download bar divides by must be the whole asset.
//
// The two halves are tested separately elsewhere — this pins that they are
// actually connected, and that the total counts every representation rather than
// one of them.
func TestProcessMPDPublishesStaticTotal(t *testing.T) {
	data, err := os.ReadFile("../../testdata/manifest.mpd")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	p, err := imdp.Parse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Type != imdp.PresentationStatic {
		t.Fatalf("fixture is %q, want a static MPD", p.Type)
	}

	w, prog := newProgressWatcher(t)
	w.processMPD(context.Background(), p)

	snap := prog.Snapshot()
	if !snap.TotalKnown {
		t.Error("TotalKnown = false after processing a static MPD")
	}
	// testdata/manifest.mpd is 5 representations × 15 segments.
	if snap.ExpectedSegments != 75 {
		t.Errorf("ExpectedSegments = %d, want 75", snap.ExpectedSegments)
	}
	if snap.TotalCapped {
		t.Error("TotalCapped = true for an ordinary manifest — this would suppress the ETA")
	}
	// Discovery must not have been mistaken for stored progress.
	if snap.SegmentsStored != 0 {
		t.Errorf("SegmentsStored = %d before anything was fetched, want 0", snap.SegmentsStored)
	}
}

// TestProcessMPDPublishesNoTotalForDynamic is the watcher-level half of the rule
// that a live stream gets no denominator. Setting one here is what produced the
// permanently-wrong percentage this reporting replaced.
func TestProcessMPDPublishesNoTotalForDynamic(t *testing.T) {
	p := &imdp.ParsedMPD{
		Type:                  imdp.PresentationDynamic,
		AvailabilityStartTime: time.Now().Add(-8 * time.Second),
		TimeShiftBufferDepth:  30 * time.Second,
		Periods: []*imdp.ParsedPeriod{{
			ID: "p0",
			AdaptationSets: []*imdp.ParsedAdaptationSet{{
				ID:        "v0",
				MediaType: imdp.MediaVideo,
				Representations: []*imdp.ParsedRepresentation{{
					ID: "r0",
					SegTemplate: &imdp.ParsedSegmentTemplate{
						Timescale:   1,
						Duration:    2,
						StartNumber: 100,
						Media:       "seg-$Number$.m4s",
						Init:        "init.mp4",
					},
				}},
			}},
		}},
	}

	w, prog := newProgressWatcher(t)
	w.processMPD(context.Background(), p)

	snap := prog.Snapshot()
	if snap.TotalKnown || snap.ExpectedSegments != 0 {
		t.Errorf("live stream got a total: TotalKnown=%v ExpectedSegments=%d, want false/0",
			snap.TotalKnown, snap.ExpectedSegments)
	}
	if _, ok := snap.ETA(); ok {
		t.Error("live stream produced an ETA, want none")
	}
}

// TestProcessMPDFlagsTruncatedTotal pins the remaining link: when discovery
// truncates its enumeration, the total reaching the counters is marked as a floor
// so the UI suppresses the percentage and ETA derived from it.
func TestProcessMPDFlagsTruncatedTotal(t *testing.T) {
	// A static period long enough that a single representation blows past
	// maxSegmentsPerPass, which is what forces the truncation.
	p := &imdp.ParsedMPD{
		Type: imdp.PresentationStatic,
		Periods: []*imdp.ParsedPeriod{{
			ID:       "p0",
			Duration: 1_000_000 * time.Hour,
			AdaptationSets: []*imdp.ParsedAdaptationSet{{
				ID:        "v0",
				MediaType: imdp.MediaVideo,
				Representations: []*imdp.ParsedRepresentation{{
					ID: "r0",
					SegTemplate: &imdp.ParsedSegmentTemplate{
						Timescale:   90000,
						Duration:    90000,
						StartNumber: 1,
						Media:       "seg-$Number$.m4s",
						Init:        "init.mp4",
					},
				}},
			}},
		}},
	}

	w, prog := newProgressWatcher(t)
	w.processMPD(context.Background(), p)

	snap := prog.Snapshot()
	if !snap.TotalCapped {
		t.Error("TotalCapped = false after a truncated enumeration, want true")
	}
	if snap.ExpectedSegments != maxSegmentsPerPass {
		t.Errorf("ExpectedSegments = %d, want the cap %d", snap.ExpectedSegments, maxSegmentsPerPass)
	}
	// A floor total must never yield an ETA, however healthy the rate looks.
	prog.AddStored(1000, 1000)
	if d, ok := snap.ETA(); ok {
		t.Errorf("ETA = %v from a capped total, want none", d)
	}
}
