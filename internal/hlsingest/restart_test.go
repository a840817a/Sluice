package hlsingest

import (
	"context"
	"testing"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/queue"
)

// heldSegments describes what a channel's index holds when the gateway starts:
// for each period, the segment numbers each representation has on disk. Every
// segment is 6 s, placed on its period's timeline by its number, so the first
// one listed starts at PTS 0 and a missing number leaves a hole.
type heldSegments map[string]map[string][]uint64

// restartedWatcher builds a watcher over an index that already holds segs, the
// way a gateway restart finds a channel that was ingesting before it went down.
func restartedWatcher(segs heldSegments) *Watcher {
	ci := index.NewChannelIndex()
	for periodID, reps := range segs {
		for repID, nums := range reps {
			v, mediaType := testVariant(), "video"
			if repID == audioVariant().RepID {
				v, mediaType = audioVariant(), "audio"
			}
			asID := v.asID
			as := ci.Period(periodID).AS(asID)
			as.SetMediaType(mediaType)
			rep := as.Rep(repID, 0)
			for _, n := range nums {
				start := int64(n-nums[0]) * 6 * TSTimescale
				rep.RestorePublished(index.SegmentState{
					SegNo: n, StartPTS: start, EndPTS: start + 6*TSTimescale, Timescale: TSTimescale,
				})
			}
		}
	}
	return NewWatcher(config.Config{}, "ch1", "https://origin.example.com/live/master.m3u8",
		nil, false, "", ci, queue.NewBroker(4096))
}

func TestRestartIngestsIntoTheLatestPeriod(t *testing.T) {
	cases := []struct {
		name string
		held heldSegments
		// poll is the first playlist the origin serves after the restart.
		pollStart uint64
		pollLen   int
		// wantPeriod is where every new segment must land.
		wantPeriod string
		// wantSegs are the segment numbers that must be fetched.
		wantSegs []uint64
		// wantStartPTS is the timeline position of the first wanted segment,
		// if any.
		wantStartPTS int64
	}{
		{
			// The origin kept its numbering while the gateway was down. Ingest
			// carries on in the period it was in, right after what it holds.
			name: "no origin reset continues the latest period",
			held: heldSegments{
				"0": {"v0": {100, 101, 102}},
				"1": {"v0": {5, 6, 7}},
			},
			pollStart: 6, pollLen: 4, // 6..9
			wantPeriod:   "1",
			wantSegs:     []uint64{8, 9},
			wantStartPTS: 3 * 6 * TSTimescale,
		},
		{
			// The origin restarted its numbering while the gateway was down, so
			// the first playlist sits wholly below what the latest period holds.
			// That is a new period, numbered above every existing one, starting
			// its own timeline.
			name: "origin reset below the held maximum opens a new period",
			held: heldSegments{
				"0": {"v0": {100, 101, 102}},
				"1": {"v0": {50, 51, 52}},
			},
			pollStart: 0, pollLen: 3, // 0..2
			wantPeriod:   "2",
			wantSegs:     []uint64{0, 1, 2},
			wantStartPTS: 0,
		},
		{
			// A segment that failed before the gateway went down is still in the
			// origin's window. Numbers below the held maximum are not a reset
			// when the playlist reaches past it: the hole is backfilled in place.
			name: "hole in the latest period is backfilled, not a reset",
			held: heldSegments{
				"0": {"v0": {100, 101, 102}},
				"1": {"v0": {50, 52}},
			},
			pollStart: 50, pollLen: 4, // 50..53
			wantPeriod:   "1",
			wantSegs:     []uint64{51, 53},
			wantStartPTS: 1 * 6 * TSTimescale,
		},
		{
			// The gateway came back before the origin published anything new.
			// A playlist ending exactly at the held maximum is not a reset.
			name: "playlist ending at the held maximum is not a reset",
			held: heldSegments{
				"0": {"v0": {100, 101, 102}},
				"1": {"v0": {50, 51, 52}},
			},
			pollStart: 50, pollLen: 3, // 50..52
			wantPeriod: "1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := restartedWatcher(tc.held)
			v := testVariant()
			st := w.newVariantState(v)

			w.enqueueNew(context.Background(), v, st, mediaPlaylist(tc.pollStart, tc.pollLen, 6.0))
			got := drain(w.broker)

			if len(got) != len(tc.wantSegs) {
				t.Fatalf("fetched segments %v, want %v", keysOf(got), tc.wantSegs)
			}
			for _, n := range tc.wantSegs {
				task, ok := got[n]
				if !ok {
					t.Fatalf("segment %d not fetched; got %v", n, keysOf(got))
				}
				if task.PeriodID != tc.wantPeriod {
					t.Errorf("segment %d landed in period %q, want %q", n, task.PeriodID, tc.wantPeriod)
				}
			}
			if len(tc.wantSegs) > 0 {
				if first := got[tc.wantSegs[0]]; first.StartPTS != tc.wantStartPTS {
					t.Errorf("segment %d StartPTS = %d, want %d", tc.wantSegs[0], first.StartPTS, tc.wantStartPTS)
				}
			}
			if p := w.PeriodID(); p != tc.wantPeriod {
				t.Errorf("channel PeriodID() = %q, want %q", p, tc.wantPeriod)
			}
		})
	}
}

// Every rendition starts its poll loop at startup and notices a reset that
// happened during downtime on its own first poll. They must all move into the
// same new period, as they do for a reset seen while running.
func TestRestartResetPutsAllRenditionsInOnePeriod(t *testing.T) {
	w := restartedWatcher(heldSegments{
		"0": {"v0": {100, 101, 102}, "a0": {100, 101, 102}},
		"1": {"v0": {50, 51, 52}, "a0": {50, 51, 52}},
	})
	video, audio := testVariant(), audioVariant()
	vState, aState := w.newVariantState(video), w.newVariantState(audio)

	w.enqueueNew(context.Background(), video, vState, mediaPlaylist(0, 3, 6.0))
	w.enqueueNew(context.Background(), audio, aState, mediaPlaylist(0, 3, 6.0))

	got := periodsByRep(w.broker)
	videoPeriod, audioPeriod := onlyKey(t, got["v0"]), onlyKey(t, got["a0"])
	if videoPeriod != "2" || audioPeriod != "2" {
		t.Errorf("video landed in period %q and audio in %q, want both in \"2\"", videoPeriod, audioPeriod)
	}
}

// An empty playlist says nothing about the origin's numbering, so it must not
// use up the first-poll check: the reset is still detected on the next poll.
func TestRestartResetDetectedAfterAnEmptyFirstPlaylist(t *testing.T) {
	w := restartedWatcher(heldSegments{
		"0": {"v0": {100, 101, 102}},
		"1": {"v0": {50, 51, 52}},
	})
	v := testVariant()
	st := w.newVariantState(v)

	w.enqueueNew(context.Background(), v, st, mediaPlaylist(0, 0, 6.0))
	w.enqueueNew(context.Background(), v, st, mediaPlaylist(0, 3, 6.0))

	if got := onlyKey(t, periodsByRep(w.broker)["v0"]); got != "2" {
		t.Errorf("segments landed in period %q, want \"2\"", got)
	}
}
