package channel

import (
	"sort"
	"time"

	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/progress"
)

// ProgressStats reports what a channel has ingested since it was last started,
// and is the data the admin UI's progress display is built on.
//
// Two distinctions in here are load-bearing, because getting either wrong means
// showing an operator a number that is confidently false:
//
//   - TotalSegments is what the source said it contains, and is only ever set for
//     a bounded source. A live channel leaves it zero and TotalKnown false: it has
//     no total, and the UI must show a cumulative count rather than invent a
//     denominator. TotalCapped means the total is a floor, so no percentage or ETA
//     may be derived from it.
//   - SegmentsFailed counts failed *attempts*, most of which are retried and
//     succeed. SegmentsDropped counts work genuinely abandoned. Only the latter
//     explains a gap between SegmentsStored and TotalSegments.
type ProgressStats struct {
	SegmentsStored  uint64 `json:"segments_stored"`
	SegmentsFailed  uint64 `json:"segments_failed"`
	SegmentsDropped uint64 `json:"segments_dropped"`
	BytesStored     uint64 `json:"bytes_stored"`

	TotalSegments uint64 `json:"total_segments"`
	TotalKnown    bool   `json:"total_known"`
	TotalCapped   bool   `json:"total_capped"`
	// Finalized reports that ingest is over, which is what separates
	// "500 of 512, still working" from "finished, 12 never arrived".
	Finalized bool `json:"finalized"`

	// QueueDepth is how many segments are waiting to be fetched right now. It is
	// the clearest single signal of whether a channel is actually moving.
	QueueDepth int `json:"queue_depth"`

	SegmentsPerSec float64 `json:"segments_per_sec"`
	BytesPerSec    float64 `json:"bytes_per_sec"`
	// ETASeconds is present only when it can be computed honestly: an exact
	// total, forward progress, and ingest still running.
	ETASeconds *float64 `json:"eta_seconds,omitempty"`

	StartedAt time.Time `json:"started_at"`
	// LastSegmentAt is absent until the channel stores its first segment.
	LastSegmentAt *time.Time `json:"last_segment_at,omitempty"`

	// SourceError is a live condition: it appears while the upstream manifest or
	// playlist is failing and clears on the next successful poll.
	SourceError *progress.ErrorRecord `json:"source_error,omitempty"`
	// LastSegmentError is sticky, so a UI polling every couple of seconds cannot
	// miss a failure that happened between polls.
	LastSegmentError *progress.ErrorRecord `json:"last_segment_error,omitempty"`
}

// buildProgress snapshots a runtime's counters. Returns nil when the channel has
// none, which keeps the field absent for a channel that is not running.
func buildProgress(rt *Runtime) *ProgressStats {
	if rt.Progress == nil {
		return nil
	}
	snap := rt.Progress.Snapshot()

	ps := &ProgressStats{
		SegmentsStored:   snap.SegmentsStored,
		SegmentsFailed:   snap.SegmentsFailed,
		BytesStored:      snap.BytesStored,
		TotalSegments:    snap.ExpectedSegments,
		TotalKnown:       snap.TotalKnown,
		TotalCapped:      snap.TotalCapped,
		Finalized:        snap.Finalized,
		SegmentsPerSec:   snap.SegmentsPerSec,
		BytesPerSec:      snap.BytesPerSec,
		StartedAt:        snap.StartedAt,
		SourceError:      snap.SourceError,
		LastSegmentError: snap.LastSegmentError,
	}
	if !snap.LastSegmentAt.IsZero() {
		t := snap.LastSegmentAt
		ps.LastSegmentAt = &t
	}
	if rt.Broker != nil {
		ps.QueueDepth = rt.Broker.Len()
		ps.SegmentsDropped = rt.Broker.Dropped()
	}
	if eta, ok := snap.ETA(); ok {
		secs := eta.Seconds()
		ps.ETASeconds = &secs
	}
	return ps
}

// TrackStats describes one representation: what it is, and how much of it the
// gateway holds. The descriptive fields come from the upstream manifest and the
// counts from the index, so a track appears here even when the manifest no longer
// mentions it — segments already on disk are still served.
type TrackStats struct {
	RepID     string `json:"rep_id"`
	PeriodID  string `json:"period_id"`
	ASID      string `json:"as_id"`
	MediaType string `json:"media_type,omitempty"`

	Codecs    string  `json:"codecs,omitempty"`
	Bandwidth uint64  `json:"bandwidth,omitempty"`
	Width     int     `json:"width,omitempty"`
	Height    int     `json:"height,omitempty"`
	FrameRate float64 `json:"frame_rate,omitempty"`

	Language string `json:"language,omitempty"`
	Name     string `json:"name,omitempty"`
	Channels string `json:"channels,omitempty"`
	Default  bool   `json:"default,omitempty"`
	Forced   bool   `json:"forced,omitempty"`

	Published int    `json:"published"`
	Committed int    `json:"committed"`
	Expired   int    `json:"expired"`
	MaxSegNo  uint64 `json:"max_seg_no"`
}

// buildTracks assembles the per-track breakdown for a channel.
//
// The index is the source of truth for which tracks exist, not the manifest: a
// representation that has been dropped upstream still has segments on disk and
// still serves, so it must remain visible. Manifest data only decorates.
func buildTracks(rt *Runtime, ci *index.ChannelIndex) []TrackStats {
	var tracks []TrackStats
	ci.ForEachRep(func(ref index.RepRef, rep *index.RepresentationState) {
		p, c, e := rep.StatusCounts()
		tracks = append(tracks, TrackStats{
			RepID:     ref.RepID,
			PeriodID:  ref.PeriodID,
			ASID:      ref.ASID,
			MediaType: ref.MediaType,
			Published: p,
			Committed: c,
			Expired:   e,
			MaxSegNo:  rep.MaxSegNo(),
		})
	})
	if len(tracks) == 0 {
		return nil
	}

	if rt.HLS != nil {
		decorateHLSTracks(rt, tracks)
	} else if p := rt.ParsedMPD(); p != nil {
		decorateDASHTracks(p, tracks)
	}

	sortTracks(tracks)
	return tracks
}

// decorateHLSTracks fills in codec, resolution and language from the discovered
// HLS source description. RepIDs are unique per channel across variants and
// renditions, so a single pass over each kind is enough.
func decorateHLSTracks(rt *Runtime, tracks []TrackStats) {
	variants := make(map[string]hlsdesc.Variant)
	for _, v := range rt.HLS.Variants() {
		variants[v.RepID] = v
	}
	audio := make(map[string]hlsdesc.Rendition)
	for _, a := range rt.HLS.Renditions() {
		audio[a.RepID] = a
	}
	subs := make(map[string]hlsdesc.Subtitle)
	for _, s := range rt.HLS.Subtitles() {
		subs[s.RepID] = s
	}

	for i := range tracks {
		t := &tracks[i]
		if v, ok := variants[t.RepID]; ok {
			t.Codecs = v.Codecs
			t.Bandwidth = v.Bandwidth
			if v.AverageBandwidth > 0 && t.Bandwidth == 0 {
				t.Bandwidth = v.AverageBandwidth
			}
			t.Width, t.Height, t.FrameRate = v.Width, v.Height, v.FrameRate
			continue
		}
		if a, ok := audio[t.RepID]; ok {
			t.Name, t.Language, t.Default, t.Channels = a.Name, a.Language, a.Default, a.Channels
			continue
		}
		if s, ok := subs[t.RepID]; ok {
			t.Name, t.Language, t.Default, t.Forced = s.Name, s.Language, s.Default, s.Forced
		}
	}
}

// decorateDASHTracks fills in codec, bitrate and resolution from the upstream
// MPD. A DASH RepID is only unique within its AdaptationSet, so the lookup is
// keyed on the full period/AS/rep triple.
func decorateDASHTracks(p *imdp.ParsedMPD, tracks []TrackStats) {
	type key struct{ periodID, asID, repID string }
	reps := make(map[key]*imdp.ParsedRepresentation)
	langs := make(map[key]string)

	for _, period := range p.Periods {
		for _, as := range period.AdaptationSets {
			for _, rep := range as.Representations {
				k := key{period.ID, as.ID, rep.ID}
				reps[k] = rep
				langs[k] = as.Lang
			}
		}
	}

	for i := range tracks {
		t := &tracks[i]
		k := key{t.PeriodID, t.ASID, t.RepID}
		rep, ok := reps[k]
		if !ok {
			continue
		}
		t.Codecs = rep.Codecs
		t.Bandwidth = rep.Bandwidth
		t.Width = int(rep.Width)
		t.Height = int(rep.Height)
		t.Language = langs[k]
	}
}

// sortTracks gives the list a stable order, so a UI diffing rows every couple of
// seconds does not see them shuffle: Go randomises map iteration, and ForEachRep
// walks maps.
func sortTracks(tracks []TrackStats) {
	sort.Slice(tracks, func(i, j int) bool {
		a, b := tracks[i], tracks[j]
		if a.PeriodID != b.PeriodID {
			return a.PeriodID < b.PeriodID
		}
		if a.ASID != b.ASID {
			return a.ASID < b.ASID
		}
		return a.RepID < b.RepID
	})
}
