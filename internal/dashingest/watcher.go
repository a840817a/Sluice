package dashingest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/progress"
	"github.com/a840817a/sluice/internal/queue"
	"github.com/a840817a/sluice/internal/validate"
)

// Watcher polls the origin's MPD and enqueues new segment tasks.
type Watcher struct {
	cfg       config.Config
	ci        *index.ChannelIndex
	broker    *queue.Broker
	channelID string

	// headers are the channel's FetchHeaders, applied to every origin request
	// this watcher makes. The provider is shared with the channel's fetcher and
	// swapped atomically by the admin API, so rotating origin credentials does
	// not require restarting the channel.
	headers fetch.Headers

	// prog records ingest progress for the admin UI. Nil is valid and means
	// "record nothing" — every Counters method tolerates a nil receiver.
	prog *progress.Counters

	mu           sync.Mutex
	lastMPD      *imdp.ParsedMPD
	lastMPDBytes []byte // raw XML bytes of the most recently fetched MPD
	// lastKnown tracks the highest seen segment number per representation key.
	lastKnown map[string]uint64
	// generations maps an origin period ID to the restart generation of the
	// gateway period its segments currently go to (ADR-0007).
	generations map[string]int
	// lastAST is the availabilityStartTime of the last dynamic MPD processed.
	lastAST time.Time
	client  *http.Client
}

// SetProgress attaches the channel's progress counters. Must be called before Run.
func (w *Watcher) SetProgress(p *progress.Counters) { w.prog = p }

// NewWatcher creates a Watcher for the given channel.
// lastKnown is seeded from ci so that segments already on disk are not
// re-downloaded, while segments missed during downtime (still in the
// origin's DVR window) are discovered and fetched on the first MPD poll.
//
// headers carries the channel's FetchHeaders. They are needed here and not only
// in the segment fetcher: an origin that requires an auth header for segments
// almost always requires it for the manifest too.
func NewWatcher(cfg config.Config, channelID string, headers fetch.Headers, ci *index.ChannelIndex, broker *queue.Broker) *Watcher {
	lk, _ := snapshotTrackedSegments(channelID, ci)

	return &Watcher{
		cfg:         cfg,
		ci:          ci,
		broker:      broker,
		channelID:   channelID,
		headers:     headers,
		lastKnown:   lk,
		generations: make(map[string]int),
		client:      &http.Client{Timeout: 10 * time.Second},
	}
}

// applyOriginHeaders sets the channel's FetchHeaders followed by the
// gateway-wide auth header, so a gateway-level credential wins over a
// same-named per-channel one. This matches hlsingest.Watcher.fetch.
func (w *Watcher) applyOriginHeaders(req *http.Request) {
	fetch.ApplyHeaders(req, w.headers)
	if w.cfg.Upstream.AuthHeader != "" {
		req.Header.Set(w.cfg.Upstream.AuthHeader, w.cfg.Upstream.AuthValue)
	}
}

// Run starts the watcher loop. It fetches the MPD once immediately, then
// re-polls according to MinimumUpdatePeriod (dynamic) or returns after
// the first fetch (static).
func (w *Watcher) Run(ctx context.Context) error {
	p, err := w.fetchMPD(ctx)
	if err != nil {
		w.prog.SetSourceError("manifest", err.Error())
		return fmt.Errorf("initial MPD fetch: %w", err)
	}
	w.prog.ClearSourceError()
	w.processMPD(ctx, p)

	if p.Type == imdp.PresentationStatic {
		return nil // static: one-shot
	}

	// Dynamic: poll at MinimumUpdatePeriod.
	interval := p.MinimumUpdatePeriod
	if interval <= 0 {
		interval = w.cfg.Upstream.PollInterval
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			p, err := w.fetchMPD(ctx)
			if err != nil {
				slog.Warn("MPD poll failed",
					"channel", w.channelID,
					"url", w.cfg.Upstream.MPDURL,
					"err", err,
				)
				// A failing origin is a live condition, not a historical event:
				// it is cleared as soon as a poll succeeds, so the admin UI shows
				// "degraded now" rather than "was unhappy at some point".
				w.prog.SetSourceError("manifest", err.Error())
				continue
			}
			w.prog.ClearSourceError()
			w.processMPD(ctx, p)
			// The stream ended: the origin now describes a finished asset.
			// Returning nil is how a source reports "ingest complete" — the
			// channel manager turns that into the VOD transition, the same way
			// it does for an HLS source whose variants all reach ENDLIST.
			// processMPD ran first on purpose: the final static MPD names the
			// whole timeline, so any tail segments the last dynamic MPD had
			// already rolled out of its window get enqueued before we stop.
			if p.Type == imdp.PresentationStatic {
				slog.Info("upstream stream ended", "channel", w.channelID)
				return nil
			}
			// Adjust interval if the MPD changed it.
			newInterval := p.MinimumUpdatePeriod
			if newInterval <= 0 {
				newInterval = w.cfg.Upstream.PollInterval
			}
			if newInterval > 0 && newInterval != interval {
				interval = newInterval
				ticker.Reset(interval)
			}
		}
	}
}

// UpstreamType reports the presentation type of the most recently fetched MPD
// ("dynamic" or "static"), or "" before the first fetch.
//
// It is the DASH answer to the same question EndListProgress answers for HLS:
// has the origin declared the stream finished yet.
func (w *Watcher) UpstreamType() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lastMPD == nil {
		return ""
	}
	return string(w.lastMPD.Type)
}

// ParsedMPD returns the most recently fetched ParsedMPD (nil if none yet).
func (w *Watcher) ParsedMPD() *imdp.ParsedMPD {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastMPD
}

// ParsedMPDBytes returns the raw XML bytes of the most recently fetched MPD.
func (w *Watcher) ParsedMPDBytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastMPDBytes
}

// SetParsedMPD injects a pre-parsed MPD and its raw bytes without fetching.
// Used when restoring a VOD channel from disk so ParsedMPD() is non-nil
// even though the watcher never runs.
func (w *Watcher) SetParsedMPD(p *imdp.ParsedMPD, raw []byte) {
	w.mu.Lock()
	for _, period := range p.Periods {
		period.ID = index.RestartPeriodID(period.ID, w.generation(period.ID))
	}
	w.lastMPD = p
	w.lastMPDBytes = raw
	w.mu.Unlock()
}

func (w *Watcher) fetchMPD(ctx context.Context) (*imdp.ParsedMPD, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.cfg.Upstream.MPDURL, nil)
	if err != nil {
		return nil, err
	}
	w.applyOriginHeaders(req)
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream MPD status %d", resp.StatusCode)
	}
	data, err := fetch.ReadAtMost(resp.Body, fetch.MaxManifestBytes)
	if err != nil {
		return nil, err
	}
	p, err := imdp.Parse(data)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.lastMPDBytes = data
	w.mu.Unlock()
	return p, nil
}

func (w *Watcher) processMPD(ctx context.Context, p *imdp.ParsedMPD) {
	presentMax, knownSegs := snapshotTrackedSegments(w.channelID, w.ci)

	w.mu.Lock()
	w.adoptPeriods(p, presentMax)
	w.lastMPD = p
	snapshot := make(map[string]uint64, len(w.lastKnown))
	for k, v := range w.lastKnown {
		snapshot[k] = v
	}
	w.mu.Unlock()

	// Prefer MPD-level BaseURL over computed directory URL.
	baseURL := mpdDirURL(w.cfg.Upstream.MPDURL)
	if p.BaseURL != "" {
		baseURL = resolveBaseURL(baseURL, p.BaseURL)
	}

	// Collect tasks from SegmentTemplate and SegmentList representations.
	tasks, truncated := DiscoverTasks(p, baseURL, w.channelID, snapshot, presentMax, knownSegs)

	// Collect tasks from SegmentBase representations (requires sidx fetch).
	tasks = append(tasks, w.discoverSegmentBaseTasks(ctx, p, baseURL, snapshot, presentMax, knownSegs)...)

	// A static MPD names its whole asset in one pass, and static discovery does
	// not filter out what is already on disk, so the task count here is the
	// channel's complete segment count — the only honest denominator for a
	// download bar. A dynamic MPD deliberately gets none: a live stream has no
	// total, and inventing one is the bug this reporting replaces.
	if p.Type == imdp.PresentationStatic {
		w.prog.SetExpectedTotal(uint64(len(tasks)))
		if truncated {
			w.prog.MarkCapped()
		}
	}

	// Feed the broker in presentation order rather than one representation at a
	// time. Discovery walks period → AdaptationSet → Representation, so its
	// natural output is every segment of the first representation, then the
	// next — and while the whole asset fits in the queue that does not matter,
	// because the broker is a priority queue and pops by segment number anyway.
	//
	// A bounded queue removes that safety net: what has not been pushed cannot
	// be chosen, so a representation-at-a-time feed leaves the queue holding
	// only the first representation while the rest of the asset waits. Audio,
	// discovered last, would not start until the video was nearly done — and
	// with no audio to align against, the A/V barrier publishes nothing, so an
	// ingest that is visibly progressing stays unplayable until the end.
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].Priority < tasks[j].Priority })

	for _, t := range tasks {
		if p.Type == imdp.PresentationStatic {
			if !w.broker.PushPreserving(ctx, t) {
				return // context cancelled mid-enumeration; Run reports it
			}
		} else {
			w.broker.Push(t)
		}
		// Update lastKnown using the same rep-level key format as DiscoverTasks.
		rk := t.RepKey()
		w.mu.Lock()
		if t.SegNo > w.lastKnown[rk] {
			w.lastKnown[rk] = t.SegNo
		}
		w.mu.Unlock()
	}

	// Ensure index has period state for each period in the MPD.
	for _, period := range p.Periods {
		w.ci.Period(period.ID)
	}
}

// adoptPeriods renames p's periods to the gateway periods their segments go
// to, starting a new one for each period the origin has restarted (ADR-0007).
//
// A gateway period's identity is the origin's Period ID only until the origin
// restarts its numbering inside that Period. Left under the same ID, the
// restarted stream's segments land in the period that holds the old ones:
// lower numbers are taken for holes and backfilled into it, higher ones are
// appended after it with timestamps that run backwards, and its playlists
// splice the two streams together.
//
// A dynamic origin period has restarted when any of its representations
// lists a highest segment number below one the gateway already holds or has
// queued for it — a live edge never moves backwards — or when the MPD's
// availabilityStartTime changed since the last MPD this watcher processed.
// The second catches a restart that kept its numbering, but only while the
// gateway runs: the previous value is not persisted.
//
// Caller holds w.mu.
func (w *Watcher) adoptPeriods(p *imdp.ParsedMPD, presentMax map[string]uint64) {
	dynamic := p.Type == imdp.PresentationDynamic
	astChanged := dynamic && !w.lastAST.IsZero() && !p.AvailabilityStartTime.Equal(w.lastAST)
	if dynamic {
		w.lastAST = p.AvailabilityStartTime
	}
	for _, period := range p.Periods {
		origin := period.ID
		gen := w.generation(origin)
		id := index.RestartPeriodID(origin, gen)
		if dynamic && w.restarted(p, period, id, presentMax, astChanged) {
			gen++
			w.generations[origin] = gen
			id = index.RestartPeriodID(origin, gen)
			slog.Info("origin restarted its numbering; starting a new period",
				"channel", w.channelID, "origin_period", origin, "period", id)
		}
		period.ID = id
	}
}

// generation returns the restart generation origin's segments currently go
// to. The first time an origin period is seen it is the latest generation the
// index already holds, so a gateway restart resumes the period it left off in
// rather than writing a restarted stream back into the old one.
//
// Caller holds w.mu.
func (w *Watcher) generation(origin string) int {
	if gen, ok := w.generations[origin]; ok {
		return gen
	}
	gen := 0
	if w.ci != nil {
		for _, id := range w.ci.PeriodIDs() {
			if g, ok := index.RestartGeneration(id, origin); ok && g > gen {
				gen = g
			}
		}
	}
	if w.generations == nil {
		w.generations = make(map[string]int)
	}
	w.generations[origin] = gen
	return gen
}

// restarted reports whether the origin has restarted period since the
// gateway started gateway period periodID for it: the gateway holds or has
// queued segments there, and either the MPD's availabilityStartTime changed
// or a representation lists a highest segment number below the highest the
// gateway holds or has queued for it.
func (w *Watcher) restarted(p *imdp.ParsedMPD, period *imdp.ParsedPeriod, periodID string, presentMax map[string]uint64, astChanged bool) bool {
	holds, movedBack := false, false
	for _, as := range period.AdaptationSets {
		for _, rep := range as.Representations {
			k := repKey(w.channelID, periodID, as.ID, rep.ID)
			held := max(presentMax[k], w.lastKnown[k])
			if held == 0 {
				continue
			}
			holds = true
			if listed, ok := highestListed(p, period, as, rep); ok && listed < held {
				movedBack = true
			}
		}
	}
	return holds && (astChanged || movedBack)
}

// highestListed returns the highest segment number the MPD lists for rep, for
// the addressing schemes that number their segments.
func highestListed(p *imdp.ParsedMPD, period *imdp.ParsedPeriod, as *imdp.ParsedAdaptationSet, rep *imdp.ParsedRepresentation) (uint64, bool) {
	if tmpl := rep.EffectiveTemplate(as); tmpl != nil {
		segs, _ := segmentNumbers(p, period, tmpl)
		if len(segs) == 0 {
			return 0, false
		}
		top := segs[0].segNo
		for _, s := range segs[1:] {
			top = max(top, s.segNo)
		}
		return top, true
	}
	if sl := rep.EffectiveSegmentList(as); sl != nil && len(sl.Segments) > 0 {
		return sl.StartNumber + uint64(len(sl.Segments)) - 1, true
	}
	return 0, false
}

// discoverSegmentBaseTasks fetches sidx boxes for all SegmentBase representations
// in the parsed MPD and returns one SegmentTask per subsegment.
func (w *Watcher) discoverSegmentBaseTasks(
	ctx context.Context,
	p *imdp.ParsedMPD,
	mpdBase string,
	lastKnown map[string]uint64,
	presentMax map[string]uint64,
	knownSegs map[string]map[uint64]struct{},
) []*queue.SegmentTask {
	var tasks []*queue.SegmentTask
	deadline := liveDeadline(p)

	for _, period := range p.Periods {
		// Resolve period-level base URL.
		periodBase := resolveBaseURL(mpdBase, period.BaseURL)

		for _, as := range period.AdaptationSets {
			asBase := resolveBaseURL(periodBase, as.BaseURL)

			for _, rep := range as.Representations {
				sb := rep.EffectiveSegmentBase(as)
				if sb == nil || sb.IndexRange == "" {
					continue
				}

				// Resolve the file URL from the BaseURL chain.
				repBase := resolveBaseURL(asBase, rep.EffectiveBaseURL(as))
				if repBase == "" {
					slog.Warn("SegmentBase rep has no BaseURL, skipping",
						"channel", w.channelID,
						"rep", rep.ID,
					)
					continue
				}

				// Parse indexRange "start-end".
				idxStart, idxEnd, ok := parseByteRange(sb.IndexRange)
				if !ok {
					slog.Warn("SegmentBase invalid indexRange",
						"rep", rep.ID, "range", sb.IndexRange)
					continue
				}

				// Fetch the sidx bytes.
				data, err := w.fetchRange(ctx, repBase, idxStart, idxEnd)
				if err != nil {
					slog.Warn("SegmentBase sidx fetch failed",
						"channel", w.channelID,
						"rep", rep.ID,
						"url", repBase,
						"err", err,
					)
					continue
				}

				// Parse sidx to enumerate subsegments.
				timescale, _, entries, err := validate.ParseSidxBytes(data, idxEnd+1)
				if err != nil {
					slog.Warn("SegmentBase sidx parse failed",
						"rep", rep.ID, "err", err)
					continue
				}

				// Resolve init segment info.
				initURL := repBase
				var initRangeStart, initRangeEnd uint64
				if sb.InitRange != "" {
					is, ie, ok := parseByteRange(sb.InitRange)
					if ok {
						initRangeStart, initRangeEnd = is, ie
					}
				} else {
					// No initRange specified: probe the first 64 KB of the file
					// to find the moov box boundaries, avoiding a full-file download.
					const initProbeSize = 65535
					probeData, err := w.fetchRange(ctx, repBase, 0, initProbeSize)
					if err == nil {
						if s, e, ok := validate.FindMoovRange(probeData); ok {
							initRangeStart, initRangeEnd = s, e
						}
						// If moov extends beyond the probe window, leave initRangeEnd=0
						// so the fetcher falls back to a full-file download.
					}
				}

				rk := repKey(w.channelID, period.ID, as.ID, rep.ID)
				for i, entry := range entries {
					segNo := uint64(i + 1) // 1-based segment numbering
					if p.Type == imdp.PresentationDynamic &&
						!shouldFetchDynamicSegment(rk, segNo, lastKnown, presentMax, knownSegs) {
						continue
					}
					// Match SegmentTemplate/SegmentList scheduling: oldest
					// in-window segments are fetched first so they do not
					// disappear from the origin's DVR window.
					priority := int(segNo - 1)
					tasks = append(tasks, &queue.SegmentTask{
						ChannelID:      w.channelID,
						PeriodID:       period.ID,
						ASID:           as.ID,
						RepID:          rep.ID,
						MediaType:      string(as.MediaType),
						SegNo:          segNo,
						URL:            repBase,
						RangeStart:     entry.Offset,
						RangeEnd:       entry.Offset + entry.Size - 1,
						InitURL:        initURL,
						InitRangeStart: initRangeStart,
						InitRangeEnd:   initRangeEnd,
						Priority:       priority,
						Deadline:       deadline,
						Duration:       uint64(entry.Duration),
						Timescale:      timescale,
					})
				}
			}
		}
	}
	return tasks
}

func snapshotTrackedSegments(channelID string, ci *index.ChannelIndex) (map[string]uint64, map[string]map[uint64]struct{}) {
	maxByRep := make(map[string]uint64)
	segNosByRep := make(map[string]map[uint64]struct{})

	ci.ForEachRep(func(ref index.RepRef, rep *index.RepresentationState) {
		segNos, max := rep.TrackedSegNos()
		if max == 0 {
			return
		}
		rk := repKey(channelID, ref.PeriodID, ref.ASID, ref.RepID)
		maxByRep[rk] = max
		segNosByRep[rk] = segNos
	})
	return maxByRep, segNosByRep
}

// fetchRange fetches a byte range from url using the watcher's HTTP client.
func (w *Watcher) fetchRange(ctx context.Context, url string, start, end uint64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	w.applyOriginHeaders(req)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d fetching range from %s", resp.StatusCode, url)
	}
	// The requested range bounds the read: an origin that ignores Range and
	// replies with the whole file must not be buffered whole.
	return fetch.ReadAtMost(resp.Body, int64(end-start)+1)
}

// mpdDirURL returns the directory portion of an MPD URL so that relative
// segment paths are resolved against the correct base.
// "https://cdn.com/live/stream.mpd" → "https://cdn.com/live"
func mpdDirURL(mpdURL string) string {
	if i := strings.LastIndex(mpdURL, "/"); i > 0 {
		return mpdURL[:i]
	}
	return mpdURL
}

// resolveBaseURL resolves a possibly-relative next URL against base.
// If next is absolute it replaces base; if relative it is joined onto base.
// Trailing slashes are normalised away.
//
// This tracks where the manifest itself lives, which is a different job from
// discovery.go's resolveURL (the MPD BaseURL hierarchy). The trailing-slash
// trimming here is required, not incidental: the result is a base that callers
// concatenate "/"+path onto, so leaving the slash produces a doubled separator.
// See resolveURL's comment for why the pair cannot be collapsed into one.
func resolveBaseURL(base, next string) string {
	if next == "" {
		return strings.TrimRight(base, "/")
	}
	if strings.HasPrefix(next, "http://") || strings.HasPrefix(next, "https://") {
		return strings.TrimRight(next, "/")
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(next, "/")
}

// parseByteRange parses a "start-end" byte range string.
func parseByteRange(s string) (start, end uint64, ok bool) {
	idx := strings.Index(s, "-")
	if idx < 0 {
		return
	}
	var err error
	start, err = strconv.ParseUint(s[:idx], 10, 64)
	if err != nil {
		return
	}
	end, err = strconv.ParseUint(s[idx+1:], 10, 64)
	if err != nil {
		return
	}
	ok = true
	return
}
