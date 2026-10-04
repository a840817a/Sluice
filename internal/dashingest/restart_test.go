package dashingest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/queue"
)

// liveMPD is a dynamic MPD whose one representation lists count segments from
// startNumber, all already available.
func liveMPD(t *testing.T, ast time.Time, startNumber, count int) *imdp.ParsedMPD {
	t.Helper()
	p, err := imdp.Parse([]byte(fmt.Sprintf(`<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic" availabilityStartTime="%s" minimumUpdatePeriod="PT2S" timeShiftBufferDepth="PT600S">
  <Period id="p0" start="PT0S">
    <AdaptationSet mimeType="video/mp4">
      <SegmentTemplate timescale="1" startNumber="%d" initialization="init.mp4" media="$Number$.m4s">
        <SegmentTimeline><S t="0" d="2" r="%d"/></SegmentTimeline>
      </SegmentTemplate>
      <Representation id="v0" bandwidth="1"/>
    </AdaptationSet>
  </Period>
</MPD>`, ast.UTC().Format(time.RFC3339), startNumber, count-1)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newRestartWatcher(ci *index.ChannelIndex) (*Watcher, *queue.Broker) {
	broker := queue.NewBroker(10000)
	return NewWatcher(config.Config{Store: config.StoreConfig{DataDir: "/nowhere"}}, "ch", nil, ci, broker), broker
}

// drain returns the period and number of every queued task.
func drain(b *queue.Broker) map[string][]uint64 {
	out := map[string][]uint64{}
	for t := b.Pop(); t != nil; t = b.Pop() {
		out[t.PeriodID] = append(out[t.PeriodID], t.SegNo)
		b.Release()
	}
	return out
}

var ast0 = time.Now().Add(-10 * time.Minute)

// An origin that restarts its numbering inside its Period starts a new
// gateway period: the restarted stream's segments must not land in the period
// that holds the old ones (ADR-0007).
func TestOriginRestartStartsANewPeriod(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after *imdp.ParsedMPD
		want  []uint64
	}{
		// Numbering restarts, presentation restarted.
		{"renumbered", liveMPD(t, ast0.Add(time.Minute), 1, 16), seq(1, 16)},
		// The live edge moved back below what the gateway holds, which is
		// how a restart shows when the next poll comes seconds later.
		{"renumbered, same availabilityStartTime", liveMPD(t, ast0, 1, 6), seq(1, 6)},
		// Numbering kept, presentation restarted.
		{"same numbers, new availabilityStartTime", liveMPD(t, ast0.Add(time.Minute), 10, 5), seq(10, 14)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, broker := newRestartWatcher(index.NewChannelIndex(index.PublishAVBarrier))
			w.processMPD(context.Background(), liveMPD(t, ast0, 10, 5))
			if got := drain(broker); len(got["p0"]) != 5 {
				t.Fatalf("before the restart: queued %v, want 5 segments of p0", got)
			}

			w.processMPD(context.Background(), tc.after)
			got := drain(broker)
			if len(got["p0"]) != 0 {
				t.Errorf("restarted segments %v went into the old period p0", got["p0"])
			}
			if !sameNumbers(got["p0.r1"], tc.want) {
				t.Errorf("p0.r1 got %v, want %v (all queued: %v)", got["p0.r1"], tc.want, got)
			}
			if id := w.ParsedMPD().Periods[0].ID; id != "p0.r1" {
				t.Errorf("served MPD names the period %q, want p0.r1", id)
			}
		})
	}
}

// A live window moving forward, and holes below a live edge that has not
// moved back, are not restarts.
func TestNoRestartWithoutOne(t *testing.T) {
	w, broker := newRestartWatcher(index.NewChannelIndex(index.PublishAVBarrier))
	w.processMPD(context.Background(), liveMPD(t, ast0, 10, 5))
	drain(broker)
	w.processMPD(context.Background(), liveMPD(t, ast0, 10, 5)) // nothing new yet
	if got := drain(broker); len(got) != 0 {
		t.Errorf("an unchanged MPD queued %v", got)
	}
	w.processMPD(context.Background(), liveMPD(t, ast0, 12, 6)) // 12..17
	if got := drain(broker); len(got) != 1 || !sameNumbers(got["p0"], seq(15, 17)) || w.ParsedMPD().Periods[0].ID != "p0" {
		t.Errorf("sliding window queued %v, want 15..17 in p0", got)
	}

	// A period the gateway holds nothing of has nothing to restart out of,
	// even when the presentation changed.
	other := liveMPD(t, ast0.Add(time.Minute), 1, 3)
	other.Periods[0].ID = "other"
	w.processMPD(context.Background(), other)
	if got := drain(broker); len(got["other"]) != 3 {
		t.Errorf("a new period under a new availabilityStartTime queued %v, want 3 segments of other", got)
	}

	// A static MPD is a whole asset, not a live edge.
	sw, sbroker := newRestartWatcher(index.NewChannelIndex(index.PublishAVBarrier))
	p := liveMPD(t, ast0, 10, 5)
	sw.processMPD(context.Background(), p)
	drain(sbroker)
	static := liveMPD(t, ast0.Add(time.Minute), 1, 3)
	static.Type = imdp.PresentationStatic
	sw.processMPD(context.Background(), static)
	if got := drain(sbroker); len(got["p0.r1"]) != 0 {
		t.Errorf("a static MPD started a restart period: %v", got)
	}
}

// After a gateway restart the watcher resumes the latest period it had
// started, rather than writing the restarted stream back into the old one.
func TestRestartPeriodSurvivesAGatewayRestart(t *testing.T) {
	ci := index.NewChannelIndex(index.PublishAVBarrier)
	for _, id := range []string{"p0", "p0.r1"} {
		ci.Period(id).AS("0").Rep("v0", 0).RestorePublished(index.SegmentState{SegNo: 3, EndPTS: 1, Timescale: 1})
	}
	w, broker := newRestartWatcher(ci)
	w.processMPD(context.Background(), liveMPD(t, ast0, 1, 6)) // 1..6
	got := drain(broker)
	if len(got["p0"]) != 0 || len(got["p0.r2"]) != 0 || len(got["p0.r1"]) == 0 {
		t.Errorf("after a gateway restart queued %v, want only p0.r1", got)
	}

	// Restoring a VOD channel names its MPD's periods the same way.
	restored, _ := newRestartWatcher(ci)
	restored.SetParsedMPD(liveMPD(t, ast0, 1, 6), nil)
	if id := restored.ParsedMPD().Periods[0].ID; id != "p0.r1" {
		t.Errorf("restored MPD names the period %q, want p0.r1", id)
	}
}

func seq(from, to uint64) []uint64 {
	var out []uint64
	for n := from; n <= to; n++ {
		out = append(out, n)
	}
	return out
}

func sameNumbers(a, b []uint64) bool {
	set := map[uint64]int{}
	for _, n := range a {
		set[n]++
	}
	for _, n := range b {
		set[n]--
	}
	for _, c := range set {
		if c != 0 {
			return false
		}
	}
	return len(a) == len(b)
}
