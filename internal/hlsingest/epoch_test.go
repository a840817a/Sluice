package hlsingest

import (
	"testing"

	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/queue"
)

// periodsByRep drains the broker and returns the PeriodID each representation's
// tasks landed in. Two reps in different periods is the failure this guards.
func periodsByRep(b *queue.Broker) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for {
		t := b.Pop()
		if t == nil {
			return out
		}
		if out[t.RepID] == nil {
			out[t.RepID] = map[string]bool{}
		}
		out[t.RepID][t.PeriodID] = true
	}
}

func audioVariant() variant {
	return variant{
		Variant:   hlsdesc.Variant{RepID: "a0"},
		url:       "https://origin.example.com/live/a0.m3u8",
		asID:      AudioASID,
		mediaType: "audio",
		rendition: true,
		kind:      renditionAudio,
	}
}

// An upstream restart rewinds the media sequence on every rendition at once, but
// each rendition has its own poll loop and notices at a different moment. All of
// them must land in the SAME new period: a per-variant epoch would put video in
// period 1 and audio in period 2, and the synthesized MPD would then describe
// two periods that each carry only one track.
func TestStreamResetPutsAllRenditionsInOnePeriod(t *testing.T) {
	w := newTestWatcher()
	video, audio := testVariant(), audioVariant()
	vState, aState := &variantState{}, &variantState{}

	// Both renditions ingest normally in the initial period.
	w.enqueueNew(video, vState, mediaPlaylist(100, 3, 6.0))
	w.enqueueNew(audio, aState, mediaPlaylist(100, 3, 6.0))
	drain(w.broker)

	// The upstream restarts. Video's poll loop notices first, audio's a moment
	// later — the ordering here is the whole point.
	w.enqueueNew(video, vState, mediaPlaylist(0, 3, 6.0))
	w.enqueueNew(audio, aState, mediaPlaylist(0, 3, 6.0))

	got := periodsByRep(w.broker)
	if len(got["v0"]) != 1 || len(got["a0"]) != 1 {
		t.Fatalf("expected one period per rep after the reset, got %v", got)
	}
	var videoPeriod, audioPeriod string
	for p := range got["v0"] {
		videoPeriod = p
	}
	for p := range got["a0"] {
		audioPeriod = p
	}
	if videoPeriod != audioPeriod {
		t.Errorf("video landed in period %q but audio in %q; a reset must move the whole channel into one period",
			videoPeriod, audioPeriod)
	}
	if videoPeriod == "0" {
		t.Errorf("period did not advance past %q after a reset", videoPeriod)
	}
}

// A second, genuinely separate reset must still create a new period rather than
// reusing the one the first reset created.
func TestSecondResetAdvancesThePeriodAgain(t *testing.T) {
	w := newTestWatcher()
	video, audio := testVariant(), audioVariant()
	vState, aState := &variantState{}, &variantState{}

	w.enqueueNew(video, vState, mediaPlaylist(100, 3, 6.0))
	w.enqueueNew(audio, aState, mediaPlaylist(100, 3, 6.0))
	drain(w.broker)

	// First reset.
	w.enqueueNew(video, vState, mediaPlaylist(50, 3, 6.0))
	w.enqueueNew(audio, aState, mediaPlaylist(50, 3, 6.0))
	first := periodsByRep(w.broker)

	// Second reset, some time later.
	w.enqueueNew(video, vState, mediaPlaylist(10, 3, 6.0))
	w.enqueueNew(audio, aState, mediaPlaylist(10, 3, 6.0))
	second := periodsByRep(w.broker)

	firstPeriod, secondPeriod := onlyKey(t, first["v0"]), onlyKey(t, second["v0"])
	if firstPeriod == secondPeriod {
		t.Errorf("second reset reused period %q; each reset needs its own period", firstPeriod)
	}
	if a := onlyKey(t, second["a0"]); a != secondPeriod {
		t.Errorf("after the second reset video is in %q but audio in %q", secondPeriod, a)
	}
}

func onlyKey(t *testing.T, m map[string]bool) string {
	t.Helper()
	if len(m) != 1 {
		t.Fatalf("expected exactly one period, got %v", m)
	}
	for k := range m {
		return k
	}
	return ""
}
