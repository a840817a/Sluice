package index

import "sync"

// Barrier checks A/V readiness and publishes segments when both tracks are committed.
// It uses presentation time range overlap rather than exact segNo matching to
// tolerate the ~21ms audio/video duration misalignment common in AAC + H.264.
type Barrier struct {
	mu     sync.Mutex
	period *PeriodState
	// overlapTolerance is the minimum fractional overlap required (0–1).
	// 0.5 means at least 50% of the shorter segment must overlap the other.
	overlapTolerance float64
}

// NewBarrier creates a Barrier for the given period.
func NewBarrier(period *PeriodState, overlapTolerance float64) *Barrier {
	if overlapTolerance <= 0 {
		overlapTolerance = 0.5
	}
	return &Barrier{period: period, overlapTolerance: overlapTolerance}
}

// CheckAndPublish scans all AdaptationSets in the period for COMMITTED segments
// and publishes those for which the A/V barrier is satisfied.
//
// A segment slot is publishable when:
//   - For every AdaptationSet (of type video or audio), at least one
//     representation has a COMMITTED segment whose time range overlaps
//     the candidate video segment by >= overlapTolerance.
//
// All representations within a publishable slot are published together.
func (b *Barrier) CheckAndPublish() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.period.mu.RLock()
	defer b.period.mu.RUnlock()

	// Gather video and audio AdaptationSets that have at least one committed
	// segment. ASes with only published/expired segments (e.g. rebuilt history
	// from a previous session) are excluded so they do not block new segments.
	var videoASes, audioASes []*AdaptationState
	for _, as := range b.period.AdaptationSets {
		as.mu.RLock()
		var hasCommitted, hasAny bool
		for _, rep := range as.Reps {
			if len(rep.Committed()) > 0 {
				hasCommitted = true
			}
			if len(rep.Committed()) > 0 || len(rep.Published()) > 0 {
				hasAny = true
			}
		}
		mediaType := as.mediaType
		as.mu.RUnlock()
		// Skip ASes that are fully processed (all published/expired, none committed).
		// Completely empty ASes are kept — they block the barrier until data arrives.
		if hasAny && !hasCommitted {
			continue
		}
		switch mediaType {
		case MediaVideo:
			videoASes = append(videoASes, as)
		case MediaAudio:
			audioASes = append(audioASes, as)
		}
	}

	// If only one track type is present (video-only or audio-only stream),
	// publish all committed segments without A/V gating.
	if len(videoASes) == 0 || len(audioASes) == 0 {
		allASes := append(videoASes, audioASes...)
		toPublish := make(map[string]map[uint64]struct{})
		for _, as := range allASes {
			as.mu.RLock()
			for _, rep := range as.Reps {
				for _, seg := range rep.Committed() {
					if toPublish[as.ID] == nil {
						toPublish[as.ID] = make(map[uint64]struct{})
					}
					toPublish[as.ID][seg.SegNo] = struct{}{}
				}
			}
			as.mu.RUnlock()
		}
		for _, as := range allASes {
			segNos, ok := toPublish[as.ID]
			if !ok {
				continue
			}
			as.mu.RLock()
			for _, rep := range as.Reps {
				rep.MarkPublished(segNos)
			}
			as.mu.RUnlock()
		}
		return
	}

	// Collect all committed video time ranges (start/end in seconds).
	type timeRange struct {
		start, end float64
		asID       string
		segNo      uint64
	}
	var videoRanges []timeRange
	for _, as := range videoASes {
		as.mu.RLock()
		for _, rep := range as.Reps {
			for _, seg := range rep.Committed() {
				videoRanges = append(videoRanges, timeRange{
					start: seg.StartSec(),
					end:   seg.EndSec(),
					asID:  as.ID,
					segNo: seg.SegNo,
				})
			}
		}
		as.mu.RUnlock()
	}

	// For each video range, check if every audio AS has an overlapping committed segment.
	toPublish := make(map[string]map[uint64]struct{}) // asID → set of segNos

	for _, vr := range videoRanges {
		allAudioReady := true
		var audioMatches []struct {
			asID  string
			segNo uint64
		}

		for _, as := range audioASes {
			found := false
			as.mu.RLock()
			for _, rep := range as.Reps {
				for _, seg := range rep.Committed() {
					if b.overlaps(vr.start, vr.end, seg.StartSec(), seg.EndSec()) {
						audioMatches = append(audioMatches, struct {
							asID  string
							segNo uint64
						}{as.ID, seg.SegNo})
						found = true
						break
					}
				}
				if found {
					break
				}
			}
			as.mu.RUnlock()
			if !found {
				allAudioReady = false
				break
			}
		}

		if !allAudioReady {
			continue
		}

		// Publish this video segment
		if toPublish[vr.asID] == nil {
			toPublish[vr.asID] = make(map[uint64]struct{})
		}
		toPublish[vr.asID][vr.segNo] = struct{}{}

		// Publish matched audio segments
		for _, am := range audioMatches {
			if toPublish[am.asID] == nil {
				toPublish[am.asID] = make(map[uint64]struct{})
			}
			toPublish[am.asID][am.segNo] = struct{}{}
		}
	}

	// Apply publish transitions
	for _, as := range append(videoASes, audioASes...) {
		segNos, ok := toPublish[as.ID]
		if !ok {
			continue
		}
		as.mu.RLock()
		for _, rep := range as.Reps {
			rep.MarkPublished(segNos)
		}
		as.mu.RUnlock()
	}
}

// overlaps returns true if [s1,e1) and [s2,e2) overlap by at least
// overlapTolerance × min(duration1, duration2).
func (b *Barrier) overlaps(s1, e1, s2, e2 float64) bool {
	start := max(s1, s2)
	end := min(e1, e2)
	if end <= start {
		return false
	}
	overlap := end - start
	d1 := e1 - s1
	d2 := e2 - s2
	shorter := min(d1, d2)
	if shorter <= 0 {
		return false
	}
	return overlap/shorter >= b.overlapTolerance
}
