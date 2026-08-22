package dashingest

import (
	"os"
	"testing"
	"time"

	imdp "github.com/a840817a/sluice/internal/mpd"
)

func TestDiscoverTasksStatic(t *testing.T) {
	data, err := os.ReadFile("../../testdata/manifest.mpd")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	p, err := imdp.Parse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	tasks, truncated := DiscoverTasks(p, "https://origin.example.com", "ch1", nil, nil, nil)

	// 5 reps × 15 segments = 75 tasks. This is a characterization assertion
	// against testdata/manifest.mpd and the only thing pinning DiscoverTasks's
	// output cardinality — do not relax it to make a change pass.
	if len(tasks) != 75 {
		t.Errorf("expected 75 tasks, got %d", len(tasks))
	}
	// This count is also what the admin UI uses as a download denominator, so it
	// has to be reported as exact. A real manifest must never be flagged capped.
	if truncated {
		t.Error("truncated = true for testdata/manifest.mpd, want false")
	}

	// Live edge segment should have priority 0
	liveEdgeCount := 0
	for _, task := range tasks {
		if task.Priority == 0 {
			liveEdgeCount++
		}
	}
	// There are 5 reps, each with seg 15 at priority 0
	if liveEdgeCount != 5 {
		t.Errorf("expected 5 live-edge tasks, got %d", liveEdgeCount)
	}

	// Static manifests always enumerate the full segment set again.
	lastKnown := make(map[string]uint64)
	for _, task := range tasks {
		key := repKey(task.ChannelID, task.PeriodID, task.ASID, task.RepID)
		if task.SegNo > lastKnown[key] {
			lastKnown[key] = task.SegNo
		}
	}
	tasks2, _ := DiscoverTasks(p, "https://origin.example.com", "ch1", lastKnown, nil, nil)
	if len(tasks2) != len(tasks) {
		t.Errorf("expected static discovery to return %d tasks again, got %d", len(tasks), len(tasks2))
	}
}

func TestDiscoverTasksDynamicIncludesRestoreGapsBelowHighWater(t *testing.T) {
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

	rep := repKey("ch1", "p0", "v0", "r0")
	lastKnown := map[string]uint64{rep: 105}
	presentMax := map[string]uint64{rep: 104}
	knownSegs := map[string]map[uint64]struct{}{
		rep: {
			100: {},
			101: {},
			103: {},
			104: {},
		},
	}

	tasks, _ := DiscoverTasks(p, "https://origin.example.com/live", "ch1", lastKnown, presentMax, knownSegs)
	if len(tasks) != 1 {
		t.Fatalf("expected exactly one gap task, got %d", len(tasks))
	}
	if tasks[0].SegNo != 102 {
		t.Fatalf("expected gap segNo 102, got %d", tasks[0].SegNo)
	}
	if got := tasks[0].URL; got != "https://origin.example.com/live/seg-102.m4s" {
		t.Fatalf("unexpected task URL: %s", got)
	}
}
