// Package dashingest polls an upstream DASH source: it watches the MPD and
// turns what it advertises into segment download tasks.
//
// It is the DASH counterpart of internal/hlsingest. Everything downstream of
// the queue — fetching, validating, storing, indexing, publishing — is shared
// with HLS and lives in internal/pipeline, so this package holds only what is
// specific to reading a DASH manifest.
package dashingest

import (
	"log/slog"
	"math"
	"strings"
	"time"

	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/queue"
)

// DiscoverTasks extracts all segment fetch tasks from a parsed MPD.
//
// Supports SegmentTemplate (number-based and SegmentTimeline) and SegmentList.
// SegmentBase representations are handled separately in Watcher.discoverSegmentBaseTasks.
//
// For static MPDs every segment from startNumber to lastSegNo is returned.
// For dynamic MPDs, callers pass both the last scheduled high-water mark and the
// on-disk segment set. This allows startup recovery to fetch:
//   - new segments with segNo > lastKnown[repKey]
//   - gaps still inside the current MPD window but missing on disk below the
//     current on-disk high-water mark
//
// baseURL is prepended to relative segment URLs (trailing slash optional).
// channelID identifies the channel these tasks belong to.
// lastKnown maps repKey → last enqueued segNo (pass nil for first call or static).
// presentMax maps repKey → highest segment number already present on disk.
// knownSegs maps repKey → set of segment numbers already present on disk.
//
// The second result reports that at least one representation's enumeration was
// truncated at maxSegmentsPerPass, making len(tasks) a floor rather than the true
// segment count. Callers deriving a progress total from it must not present a
// percentage or an ETA, both of which would be wrong in the reassuring direction.
func DiscoverTasks(
	p *imdp.ParsedMPD,
	baseURL string,
	channelID string,
	lastKnown map[string]uint64,
	presentMax map[string]uint64,
	knownSegs map[string]map[uint64]struct{},
) ([]*queue.SegmentTask, bool) {
	if lastKnown == nil {
		lastKnown = make(map[string]uint64)
	}
	if presentMax == nil {
		presentMax = make(map[string]uint64)
	}
	if knownSegs == nil {
		knownSegs = make(map[string]map[uint64]struct{})
	}
	baseURL = strings.TrimRight(baseURL, "/")

	var tasks []*queue.SegmentTask
	// truncated is OR'd across every representation: the cap applies per rep, so
	// a multi-rep manifest can legitimately exceed it in total while each rep
	// stays under, and any single truncated rep makes the whole count a floor.
	truncated := false
	// liveEdge / startEdge bracket the segment numbers found across all reps.
	// Both are used to compute download priority (oldest segment first).
	liveEdge := uint64(0)
	startEdge := uint64(math.MaxUint64)

	// First pass: collect all (rep, segNo) pairs and find live edge.
	type entry struct {
		periodID   string
		asID       string
		repID      string
		mediaType  string
		initURL    string
		segNo      uint64
		url        string
		duration   uint64
		timescale  uint32
		rangeStart uint64
		rangeEnd   uint64 // 0 = full download
		// init byte range (SegmentList with <Initialization range="...">)
		initRangeStart uint64
		initRangeEnd   uint64
		// fallback URLs derived from alternative <BaseURL> elements
		fallbackURLs []string
	}
	var entries []entry

	for _, period := range p.Periods {
		// Resolve period-level base URL.
		periodBase := resolveURL(baseURL, period.BaseURL)

		for _, as := range period.AdaptationSets {
			asBase := resolveURL(periodBase, as.BaseURL)

			for _, rep := range as.Representations {
				repKey := repKey(channelID, period.ID, as.ID, rep.ID)

				// ── SegmentTemplate ──────────────────────────────────────────
				tmpl := rep.EffectiveTemplate(as)
				if tmpl != nil {
					repBase := resolveURL(asBase, rep.BaseURL)
					initURL := resolveURL(repBase, tmpl.InitURL(rep.ID))

					// Build fallback URLs from alt BaseURL elements (multi-CDN).
					var fallbacks []string
					for _, alt := range rep.AltBaseURLs {
						fallbacks = append(fallbacks, resolveURL(resolveURL(asBase, alt), rep.BaseURL))
					}
					if len(fallbacks) == 0 {
						for _, alt := range as.AltBaseURLs {
							fallbacks = append(fallbacks, resolveURL(alt, rep.BaseURL))
						}
					}

					segs, segsTruncated := segmentNumbers(p, period, tmpl)
					truncated = truncated || segsTruncated
					for _, ref := range segs {
						if p.Type == imdp.PresentationDynamic &&
							!shouldFetchDynamicSegment(repKey, ref.segNo, lastKnown, presentMax, knownSegs) {
							continue
						}
						if ref.segNo > liveEdge {
							liveEdge = ref.segNo
						}
						if ref.segNo < startEdge {
							startEdge = ref.segNo
						}
						segURL := resolveURL(repBase, tmpl.SegmentURL(ref.segNo, ref.segTime, rep.ID))
						var segFallbacks []string
						for _, fb := range fallbacks {
							segFallbacks = append(segFallbacks, resolveURL(fb, tmpl.SegmentURL(ref.segNo, ref.segTime, rep.ID)))
						}
						// For SegmentTimeline, tmpl.Duration is 0; use the per-S element duration instead.
						segDur := tmpl.Duration
						if segDur == 0 {
							segDur = ref.dur
						}
						entries = append(entries, entry{
							periodID:     period.ID,
							asID:         as.ID,
							repID:        rep.ID,
							mediaType:    string(as.MediaType),
							initURL:      initURL,
							segNo:        ref.segNo,
							url:          segURL,
							duration:     segDur,
							timescale:    uint32(tmpl.Timescale),
							fallbackURLs: segFallbacks,
						})
					}
					continue
				}

				// ── SegmentList ───────────────────────────────────────────────
				sl := rep.EffectiveSegmentList(as)
				if sl == nil {
					continue // SegmentBase handled separately in watcher
				}

				repBase := resolveURL(asBase, rep.EffectiveBaseURL(as))

				// Resolve init segment.
				initURL := ""
				var initRS, initRE uint64
				if sl.InitURL != "" {
					initURL = resolveURL(repBase, sl.InitURL)
				} else if sl.InitRange != "" {
					// Byte-range init within the rep BaseURL.
					initURL = repBase
					initRS, initRE, _ = parseByteRange(sl.InitRange)
				}

				for i, seg := range sl.Segments {
					segNo := sl.StartNumber + uint64(i)
					if p.Type == imdp.PresentationDynamic &&
						!shouldFetchDynamicSegment(repKey, segNo, lastKnown, presentMax, knownSegs) {
						continue
					}
					if segNo > liveEdge {
						liveEdge = segNo
					}
					if segNo < startEdge {
						startEdge = segNo
					}

					var segURL string
					var rs, re uint64
					if seg.MediaRange != "" {
						// Byte-range segment within BaseURL.
						segURL = repBase
						rs, re, _ = parseByteRange(seg.MediaRange)
					} else {
						segURL = resolveURL(repBase, seg.URL)
					}

					entries = append(entries, entry{
						periodID:       period.ID,
						asID:           as.ID,
						repID:          rep.ID,
						mediaType:      string(as.MediaType),
						initURL:        initURL,
						segNo:          segNo,
						url:            segURL,
						duration:       sl.Duration,
						timescale:      uint32(sl.Timescale),
						rangeStart:     rs,
						rangeEnd:       re,
						initRangeStart: initRS,
						initRangeEnd:   initRE,
					})
				}
			}
		}
	}

	// Second pass: build tasks with priority relative to the oldest segment.
	// Priority 0 = oldest/first segment (fetched first); higher = newer.
	// Fetching oldest first ensures:
	//   - Static VOD: segments published in order → progressive preview from segment 1.
	//   - Dynamic live: oldest at-risk segments captured before CDN expiry;
	//     live-edge segments are freshest and tolerate being fetched slightly later.
	if startEdge == math.MaxUint64 {
		startEdge = 0 // no entries; safe default
	}
	deadline := liveDeadline(p)
	for _, e := range entries {
		priority := 0
		if e.segNo > startEdge {
			priority = int(e.segNo - startEdge)
		}
		tasks = append(tasks, &queue.SegmentTask{
			ChannelID:      channelID,
			PeriodID:       e.periodID,
			ASID:           e.asID,
			RepID:          e.repID,
			MediaType:      e.mediaType,
			SegNo:          e.segNo,
			URL:            e.url,
			RangeStart:     e.rangeStart,
			RangeEnd:       e.rangeEnd,
			InitURL:        e.initURL,
			InitRangeStart: e.initRangeStart,
			InitRangeEnd:   e.initRangeEnd,
			FallbackURLs:   e.fallbackURLs,
			Priority:       priority,
			Deadline:       deadline,
			Duration:       e.duration,
			Timescale:      e.timescale,
		})
	}

	return tasks, truncated
}

// maxSegmentsPerPass bounds how many segments one discovery pass may derive for
// a single representation. Every input to that count — timeShiftBufferDepth,
// period duration, SegmentTimeline @r — comes from the upstream manifest, so a
// malformed or hostile one can otherwise ask for an allocation large enough to
// OOM the gateway and take every channel down with it. 100k segments is roughly
// two days of 2-second segments: far past any real DVR window or VOD asset,
// while still cheap to allocate.
const maxSegmentsPerPass = 100_000

// segRef pairs a segment number with its presentation start time (timescale units).
// segTime is non-zero only for SegmentTimeline-based templates ($Time$ addressing);
// number-based templates leave it at 0.
// dur is the per-S element duration (SegmentTimeline d attribute); 0 for number-based templates.
type segRef struct {
	segNo   uint64
	segTime uint64
	dur     uint64
}

// segmentNumbers enumerates one representation's segments. The second result
// reports that the enumeration hit maxSegmentsPerPass and is therefore a floor
// rather than the true count — which matters upstream, because a progress total
// derived from a floor must not be used to compute a percentage or an ETA.
func segmentNumbers(p *imdp.ParsedMPD, period *imdp.ParsedPeriod, tmpl *imdp.ParsedSegmentTemplate) ([]segRef, bool) {
	if len(tmpl.Timeline) > 0 {
		return segmentNumbersFromTimeline(p, period, tmpl)
	}
	return segmentNumbersFromDuration(p, period, tmpl)
}

func segmentNumbersFromDuration(p *imdp.ParsedMPD, period *imdp.ParsedPeriod, tmpl *imdp.ParsedSegmentTemplate) ([]segRef, bool) {
	start := tmpl.StartNumber
	var end uint64

	if p.Type == imdp.PresentationStatic {
		// Static: all segments from start to last.
		last := tmpl.LastSegmentNumber(period.Duration)
		if last == 0 || last < start {
			return nil, false
		}
		end = last
	} else {
		// Dynamic: compute live window from wall clock.
		// Do NOT guard on SegmentDuration() here — it returns 0 when Timescale is
		// absent (MPD default = 1), which would wrongly skip all tasks.
		// liveTimelineCount handles both d==0 and timescale==0 internally.
		count := liveTimelineCount(p, period, tmpl.Duration, tmpl.Timescale)
		if count == 0 {
			return nil, false
		}
		end = start + count - 1
	}

	truncated := false
	if end-start >= maxSegmentsPerPass {
		slog.Warn("upstream segment range truncated",
			"start", start, "end", end, "cap", maxSegmentsPerPass)
		end = start + maxSegmentsPerPass - 1
		truncated = true
	}

	refs := make([]segRef, 0, end-start+1)
	for n := start; n <= end; n++ {
		refs = append(refs, segRef{segNo: n})
	}
	return refs, truncated
}

func segmentNumbersFromTimeline(p *imdp.ParsedMPD, period *imdp.ParsedPeriod, tmpl *imdp.ParsedSegmentTemplate) ([]segRef, bool) {
	var refs []segRef
	segNo := tmpl.StartNumber
	curTime := uint64(0)

	for i, s := range tmpl.Timeline {
		if i == 0 || s.T != 0 {
			curTime = s.T
		}
		repeat := s.R
		if repeat < 0 {
			// Infinite repeat: treat as live dynamic count.
			segDurSec := float64(s.D) / float64(tmpl.Timescale)
			count := liveTimelineCount(p, period, s.D, tmpl.Timescale)
			if segDurSec <= 0 || count == 0 {
				break
			}
			repeat = int64(count) - 1
		}
		for range repeat + 1 {
			if uint64(len(refs)) >= maxSegmentsPerPass {
				slog.Warn("upstream SegmentTimeline truncated",
					"entries", len(tmpl.Timeline), "cap", maxSegmentsPerPass)
				return refs, true
			}
			refs = append(refs, segRef{segNo: segNo, segTime: curTime, dur: s.D})
			segNo++
			curTime += s.D
		}
	}
	return refs, false
}

func shouldFetchDynamicSegment(
	repKey string,
	segNo uint64,
	lastKnown map[string]uint64,
	presentMax map[string]uint64,
	knownSegs map[string]map[uint64]struct{},
) bool {
	if segNo > lastKnown[repKey] {
		return true
	}

	// Only treat lower segment numbers as restart gaps when they are below the
	// current on-disk high-water mark. This avoids re-enqueueing freshly
	// discovered segments that are still downloading.
	maxPresent := presentMax[repKey]
	if maxPresent == 0 || segNo > maxPresent {
		return false
	}

	segSet := knownSegs[repKey]
	if len(segSet) == 0 {
		return false
	}
	_, ok := segSet[segNo]
	return !ok
}

// liveTimelineCount returns how many segments of duration d are currently
// inside the live window.
//
// It deliberately takes no start number: availability is anchored on the period
// rather than on segment numbering — see the comment on tStartSec below.
func liveTimelineCount(p *imdp.ParsedMPD, period *imdp.ParsedPeriod, d, timescale uint64) uint64 {
	if d == 0 {
		return 0
	}
	// Guard: treat missing timescale as 1 (DASH spec default) to avoid +Inf arithmetic.
	if timescale == 0 {
		timescale = 1
	}
	segDurSec := float64(d) / float64(timescale)

	// tStartSec is the wall-clock time when the period's first segment became
	// available. For a duration-based SegmentTemplate the presentation time of
	// that segment within the period is 0, so availability = AST + period.start,
	// regardless of the actual startNumber value — do NOT add
	// (startNumber-1)*segDurSec, which wrongly assumes startNumber is
	// sequential from 1.
	var tStartSec float64
	if p.AvailabilityStartTime.IsZero() {
		tStartSec = 0
	} else {
		tStartSec = float64(p.AvailabilityStartTime.Unix()) + period.Start.Seconds()
	}

	presentationNow := float64(time.Now().Unix())
	if presentationNow <= tStartSec {
		return 1
	}
	count := uint64((presentationNow-tStartSec)/segDurSec) + 1

	// Clamp to shift buffer window.
	windowDepth := p.TimeShiftBufferDepth
	if windowDepth <= 0 {
		windowDepth = 30 * time.Second
	}
	windowSegs := uint64(math.Ceil(windowDepth.Seconds() / segDurSec))
	if count > windowSegs {
		count = windowSegs
	}
	return count
}

// liveDeadline returns the absolute deadline after which segments are stale.
// For static MPDs it returns zero (no deadline).
func liveDeadline(p *imdp.ParsedMPD) time.Time {
	if p.Type != imdp.PresentationDynamic {
		return time.Time{}
	}
	if p.TimeShiftBufferDepth > 0 {
		return time.Now().Add(p.TimeShiftBufferDepth)
	}
	// Fallback: 30 seconds
	return time.Now().Add(30 * time.Second)
}

// repKey delegates to queue.RepKey so this package's lastKnown/presentMax maps
// and the processor's per-representation maps cannot drift apart on formatting.
func repKey(channelID, periodID, asID, repID string) string {
	return queue.RepKey(channelID, periodID, asID, repID)
}

// resolveURL resolves one reference inside the MPD's BaseURL hierarchy: it
// chains period → AdaptationSet → representation bases and then produces the
// final init/segment URLs from them.
//
// Not to be confused with watcher.go's resolveBaseURL, which resolves the
// manifest's own location. The two look alike and must stay apart: this one
// preserves a trailing slash on the result (the caller may be building a base
// that a later join appends to, and DASH BaseURL semantics make "a/b/" and
// "a/b" different parents), and it has a base == "" case that resolveBaseURL
// has no use for. Merging them means one function with flags for both.
func resolveURL(base, path string) string {
	if path == "" {
		return base
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	if base == "" {
		return path
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}
