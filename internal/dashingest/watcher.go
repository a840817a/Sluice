package dashingest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
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

// Watcher polls the upstream MPD and enqueues new segment tasks.
type Watcher struct {
	cfg       config.Config
	ci        *index.ChannelIndex
	broker    *queue.Broker
	channelID string

	// headers are the channel's FetchHeaders, applied to every upstream request
	// this watcher makes. The provider is shared with the channel's fetcher and
	// swapped atomically by the admin API, so rotating upstream credentials does
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
	// onVODTransition is called once when the upstream MPD transitions from
	// dynamic to static (live stream ended). Set via SetVODTransitionCallback.
	onVODTransition func(p *imdp.ParsedMPD)

	client *http.Client
}

// SetProgress attaches the channel's progress counters. Must be called before Run.
func (w *Watcher) SetProgress(p *progress.Counters) { w.prog = p }

// NewWatcher creates a Watcher for the given channel.
// lastKnown is seeded from ci so that segments already on disk are not
// re-downloaded, while segments missed during downtime (still in the
// upstream DVR window) are discovered and fetched on the first MPD poll.
//
// headers carries the channel's FetchHeaders. They are needed here and not only
// in the segment fetcher: an origin that requires an auth header for segments
// almost always requires it for the manifest too.
func NewWatcher(cfg config.Config, channelID string, headers fetch.Headers, ci *index.ChannelIndex, broker *queue.Broker) *Watcher {
	lk, _ := snapshotTrackedSegments(channelID, ci)

	return &Watcher{
		cfg:       cfg,
		ci:        ci,
		broker:    broker,
		channelID: channelID,
		headers:   headers,
		lastKnown: lk,
		client:    &http.Client{Timeout: 10 * time.Second},
	}
}

// applyUpstreamHeaders sets the channel's FetchHeaders followed by the
// gateway-wide auth header, so a gateway-level credential wins over a
// same-named per-channel one. This matches hlsingest.Watcher.fetch.
func (w *Watcher) applyUpstreamHeaders(req *http.Request) {
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
				// A failing upstream is a live condition, not a historical event:
				// it is cleared as soon as a poll succeeds, so the admin UI shows
				// "degraded now" rather than "was unhappy at some point".
				w.prog.SetSourceError("manifest", err.Error())
				continue
			}
			w.prog.ClearSourceError()
			w.processMPD(ctx, p)
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

// SetVODTransitionCallback registers a function to be called once when the
// upstream MPD transitions from type=dynamic to type=static (stream end).
// Must be called before Run.
func (w *Watcher) SetVODTransitionCallback(fn func(p *imdp.ParsedMPD)) {
	w.mu.Lock()
	w.onVODTransition = fn
	w.mu.Unlock()
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
	w.lastMPD = p
	w.lastMPDBytes = raw
	w.mu.Unlock()
}

func (w *Watcher) fetchMPD(ctx context.Context) (*imdp.ParsedMPD, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.cfg.Upstream.MPDURL, nil)
	if err != nil {
		return nil, err
	}
	w.applyUpstreamHeaders(req)
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
	prevType := imdp.PresentationType("")
	if w.lastMPD != nil {
		prevType = w.lastMPD.Type
	}
	w.lastMPD = p
	snapshot := make(map[string]uint64, len(w.lastKnown))
	for k, v := range w.lastKnown {
		snapshot[k] = v
	}
	cb := w.onVODTransition
	w.mu.Unlock()

	// Detect live → VOD transition: upstream MPD changed from dynamic to static.
	if prevType == imdp.PresentationDynamic && p.Type == imdp.PresentationStatic && cb != nil {
		slog.Info("upstream stream ended, triggering VOD transition",
			"channel", w.channelID)
		cb(p)
		return // stop enqueueing; VOD rebuild takes over
	}

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

	for _, t := range tasks {
		w.broker.Push(t)
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
					// disappear from the upstream DVR window.
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
	w.applyUpstreamHeaders(req)
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
