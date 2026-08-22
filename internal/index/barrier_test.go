package index

import (
	"testing"
)

// makePeriod builds a PeriodState populated with committed segments.
// segs is a slice of (startPTS, endPTS) pairs in timescale units (timescale=1000).
func makePeriod(t *testing.T, asMediaTypes map[string]string, segsByAS map[string][][2]int64) *PeriodState {
	t.Helper()
	ps := newPeriodState("p0")
	for asID, mediaType := range asMediaTypes {
		as := ps.AS(asID)
		as.SetMediaType(mediaType)
		rep := as.Rep("r0", 64)
		segs, ok := segsByAS[asID]
		if !ok {
			continue
		}
		for i, s := range segs {
			rep.Commit(SegmentState{
				SegNo:     uint64(i + 1),
				StartPTS:  s[0],
				EndPTS:    s[1],
				Timescale: 1000,
			})
		}
	}
	return ps
}

func countPublished(ps *PeriodState) int {
	total := 0
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	for _, as := range ps.AdaptationSets {
		as.mu.RLock()
		for _, rep := range as.Reps {
			total += len(rep.Published())
		}
		as.mu.RUnlock()
	}
	return total
}

func TestBarrier_VideoOnly(t *testing.T) {
	ps := makePeriod(t,
		map[string]string{"v0": "video"},
		map[string][][2]int64{
			"v0": {{0, 2000}, {2000, 4000}},
		},
	)
	NewBarrier(ps, 0.5).CheckAndPublish()
	if n := countPublished(ps); n != 2 {
		t.Errorf("expected 2 published (video-only), got %d", n)
	}
}

func TestBarrier_AudioOnly(t *testing.T) {
	ps := makePeriod(t,
		map[string]string{"a0": "audio"},
		map[string][][2]int64{
			"a0": {{0, 2000}, {2000, 4000}, {4000, 6000}},
		},
	)
	NewBarrier(ps, 0.5).CheckAndPublish()
	if n := countPublished(ps); n != 3 {
		t.Errorf("expected 3 published (audio-only), got %d", n)
	}
}

func TestBarrier_VideoAudio_Overlap(t *testing.T) {
	ps := makePeriod(t,
		map[string]string{"v0": "video", "a0": "audio"},
		map[string][][2]int64{
			"v0": {{0, 2000}, {2000, 4000}},
			"a0": {{0, 2048}, {2048, 4096}},
		},
	)
	NewBarrier(ps, 0.5).CheckAndPublish()
	// Both video segs have overlapping audio → 4 published (2 video + 2 audio)
	if n := countPublished(ps); n != 4 {
		t.Errorf("expected 4 published (v+a overlap), got %d", n)
	}
}

func TestBarrier_VideoAudio_NoOverlap(t *testing.T) {
	ps := makePeriod(t,
		map[string]string{"v0": "video", "a0": "audio"},
		map[string][][2]int64{
			"v0": {{0, 1000}},
			"a0": {{5000, 6000}}, // no overlap with video
		},
	)
	NewBarrier(ps, 0.5).CheckAndPublish()
	if n := countPublished(ps); n != 0 {
		t.Errorf("expected 0 published (no overlap), got %d", n)
	}
}

func TestBarrier_MultiAudio_BothRequired(t *testing.T) {
	// Two audio ASes: video should only publish when both have overlapping audio.
	ps := makePeriod(t,
		map[string]string{"v0": "video", "a0": "audio", "a1": "audio"},
		map[string][][2]int64{
			"v0": {{0, 2000}},
			"a0": {{0, 2048}}, // overlaps video
			"a1": {{0, 2048}}, // overlaps video
		},
	)
	NewBarrier(ps, 0.5).CheckAndPublish()
	// 1 video + 1 audio(a0) + 1 audio(a1) = 3
	if n := countPublished(ps); n != 3 {
		t.Errorf("expected 3 published (multi-audio both ready), got %d", n)
	}
}

func TestBarrier_MultiAudio_OneBlocking(t *testing.T) {
	// Second audio AS has no overlapping committed segment → video blocked.
	ps := makePeriod(t,
		map[string]string{"v0": "video", "a0": "audio", "a1": "audio"},
		map[string][][2]int64{
			"v0": {{0, 2000}},
			"a0": {{0, 2048}},
			// a1 has no committed segments
		},
	)
	NewBarrier(ps, 0.5).CheckAndPublish()
	if n := countPublished(ps); n != 0 {
		t.Errorf("expected 0 published (one audio AS missing), got %d", n)
	}
}

func TestBarrier_PartialReady(t *testing.T) {
	// First video segment has matching audio; second does not.
	ps := makePeriod(t,
		map[string]string{"v0": "video", "a0": "audio"},
		map[string][][2]int64{
			"v0": {{0, 2000}, {2000, 4000}},
			"a0": {{0, 2048}}, // only covers first video segment
		},
	)
	NewBarrier(ps, 0.5).CheckAndPublish()
	// 1 video + 1 audio = 2 published
	if n := countPublished(ps); n != 2 {
		t.Errorf("expected 2 published (partial ready), got %d", n)
	}
}
