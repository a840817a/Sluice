// Package hlsingest polls a channel's HLS origin and feeds its segments into
// the same download queue, store, and index the DASH ingest path uses.
//
// It is the HLS counterpart of internal/dashingest.Watcher. Segments are never
// transformed: a .ts segment is queued, stored, and later served byte for byte.
// Because MPEG-TS carries no container-level timing this package derives each
// segment's presentation time by accumulating EXTINF durations, and hands the
// result to the processor on the task.
package hlsingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/hlskey"
	"github.com/a840817a/sluice/internal/hlssrc"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/progress"
	"github.com/a840817a/sluice/internal/queue"
)

const (
	// TSTimescale is the timescale MPEG-TS segment timing is expressed in.
	// 90 kHz is the MPEG system clock rate, so EXTINF values round-trip through
	// it with negligible error (< 1.1e-5 s) and no new duration field is needed
	// on SegmentState.
	TSTimescale = 90000

	// defaultTargetDuration is the poll interval used before a playlist has
	// advertised EXT-X-TARGETDURATION.
	defaultTargetDuration = 6 * time.Second

	// minPollInterval floors the reload rate so a pathological source cannot
	// spin the poller.
	minPollInterval = 500 * time.Millisecond

	// defaultVariantBandwidth is advertised for sources that are a bare media
	// playlist, where no BANDWIDTH was ever supplied. EXT-X-STREAM-INF requires
	// the attribute, and clients only use it to rank variants — with a single
	// variant the value is immaterial.
	defaultVariantBandwidth = 1_000_000

	// hlsMediaType is the AdaptationSet media type used for HLS variants.
	// Muxed TS variants carry audio and video together, and modelling them as a
	// single video AdaptationSet keeps the index's A/V publish barrier in its
	// single-track-type mode, where it publishes without cross-track gating.
	hlsMediaType = index.MediaVideo

	// hlsASID is the single AdaptationSet ID used for HLS channels. Exported as
	// VideoASID for the same reason AudioASID and SubtitleASID are.
	hlsASID = VideoASID

	// methodAES128 is the only EXT-X-KEY method this gateway handles. SAMPLE-AES
	// (and therefore FairPlay) is deliberately out of scope; segments carrying it
	// are passed through untouched and left for the player to deal with.
	methodAES128 = "AES-128"
)

// variant is one poll target: either a video variant or an audio rendition.
// Both are polled the same way; asID and mediaType place their segments in the
// right AdaptationSet, and the rendition fields feed EXT-X-MEDIA output.
// renditionKind classifies a demuxed rendition poll target.
type renditionKind int

const (
	renditionNone renditionKind = iota
	renditionAudio
	renditionSubtitle
)

type variant struct {
	hlsdesc.Variant
	url       string
	asID      string
	mediaType string
	// Rendition metadata, set only for demuxed audio/subtitle renditions.
	rendition bool
	kind      renditionKind
	groupID   string
	name      string
	language  string
	isDefault bool
	forced    bool
	channels  string
}

// VideoASID is the AdaptationSet ID HLS video variants are indexed under.
//
// The DASH synthesizer must key its AdaptationSet identically or the generator
// finds no segments and emits an empty manifest — a failure with no compile
// error and no test error, which is why this is a shared constant rather than
// two "these must match" comments.
const VideoASID = "0"

// AudioASID is the AdaptationSet ID demuxed audio renditions are indexed under,
// distinct from video's VideoASID. Exported so the DASH synthesizer reads the same AS.
const AudioASID = "1"

// SubtitleASID is the AdaptationSet ID demuxed WebVTT subtitle renditions are
// indexed under.
const SubtitleASID = "2"

// Watcher polls a channel's HLS origin and enqueues its segments.
type Watcher struct {
	cfg       config.Config
	ci        *index.ChannelIndex
	broker    *queue.Broker
	channelID string
	sourceURL string
	// origin shares its headers with the channel's fetcher and key cache; the
	// admin API swaps them atomically, so rotating origin credentials reaches
	// playlist polls without restarting the channel (which would reset each
	// variant's media sequence).
	origin *fetch.Origin
	client *http.Client
	// decryptOnIngest is the channel's AES-128 policy, stamped onto every
	// encrypted task so the processor never needs to know channel config.
	decryptOnIngest bool
	// statePath is where the discovered source description is persisted so a
	// restart can serve playlists without re-contacting the origin.
	statePath string

	// prog records ingest progress for the admin UI. Nil is valid and means
	// "record nothing" — every Counters method tolerates a nil receiver.
	prog *progress.Counters

	mu                  sync.Mutex
	variants            []variant
	independentSegments bool
	fmp4                bool
	// vodVariantTotals maps RepID → segment count, for poll targets that carried
	// EXT-X-ENDLIST on their very first poll. Only those are recorded: a playlist
	// that gains ENDLIST later was a live sliding window, whose final length is
	// the DVR window rather than everything the channel ingested. Publishing that
	// as a total would claim a channel had stored 12,483 of 20 segments.
	vodVariantTotals map[string]int
	// pollTargets is how many playlists Run is currently waiting on, and
	// endedTargets which of them have reported EXT-X-ENDLIST. Together they
	// answer the operator's question when a channel will not settle into VOD:
	// which playlist is still open. Kept apart from vodVariantTotals, which is
	// first-poll-only and means something else.
	pollTargets  int
	endedTargets map[string]bool
	// keyURIs maps the published key ID back to the key server URI. Only URIs
	// actually seen in this channel's playlists are ever resolvable, which is
	// what stops the key proxy from being turned into an open relay.
	keyURIs map[string]string
	// period increments whenever the origin restarts its media sequence.
	// Rather than trying to reconcile rewound segment numbers against what is
	// already indexed, a reset starts a fresh period with fresh state.
	period int
}

// NewWatcher creates a Watcher for the given channel. sourceURL may be either a
// master or a media playlist; which one it is is detected on the first fetch.
func NewWatcher(
	cfg config.Config,
	channelID, sourceURL string,
	origin *fetch.Origin,
	decryptOnIngest bool,
	statePath string,
	ci *index.ChannelIndex,
	broker *queue.Broker,
) *Watcher {
	w := &Watcher{
		cfg:             cfg,
		ci:              ci,
		broker:          broker,
		channelID:       channelID,
		sourceURL:       sourceURL,
		origin:          origin,
		decryptOnIngest: decryptOnIngest,
		statePath:       statePath,
		client:          &http.Client{Timeout: 30 * time.Second},
		keyURIs:         make(map[string]string),

		vodVariantTotals: make(map[string]int),
	}
	// Restore the previously discovered variants so playlists are serveable
	// immediately after a restart, including for channels resumed in VOD mode
	// that never run discovery again.
	w.loadState()
	w.period = latestPeriod(ci)
	return w
}

// latestPeriod returns the highest period number the index already holds,
// or 0 for an empty index. A gateway restart is not a period boundary, so
// ingest resumes in the period it was in when the gateway went down; starting
// again at 0 would append new segments to the channel's first period.
func latestPeriod(ci *index.ChannelIndex) int {
	latest := 0
	for _, id := range ci.PeriodIDs() {
		if n, err := strconv.Atoi(id); err == nil && n > latest {
			latest = n
		}
	}
	return latest
}

// registerKey records a key server URI so the gateway's key proxy can later
// resolve its published ID back to it.
func (w *Watcher) registerKey(uri string) {
	if uri == "" {
		return
	}
	id := hlskey.KeyID(uri)
	w.mu.Lock()
	_, known := w.keyURIs[id]
	w.keyURIs[id] = uri
	w.mu.Unlock()
	if !known {
		// A newly seen key must be resolvable by the proxy after a restart too.
		w.saveState()
	}
}

// KeyURIForID resolves a published key ID to the key server URI. It returns
// false for any ID that did not come from this channel's playlists.
func (w *Watcher) KeyURIForID(id string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	uri, ok := w.keyURIs[id]
	return uri, ok
}

// Description is what discovery has found at the origin, read at one
// instant.
type Description struct {
	// Variants are the video variants; Renditions and Subtitles the demuxed
	// audio and WebVTT renditions. All are empty until the first successful
	// fetch completes.
	Variants   []hlsdesc.Variant
	Renditions []hlsdesc.Rendition
	Subtitles  []hlsdesc.Subtitle
	// IndependentSegments reports whether the origin's master declared
	// EXT-X-INDEPENDENT-SEGMENTS.
	IndependentSegments bool
	// FMP4 reports whether the source carries fMP4 segments rather than
	// MPEG-TS. Only fMP4 sources can additionally be served as DASH, since TS
	// would require remuxing, which this gateway deliberately does not do.
	FMP4 bool
	// Ready reports whether discovery has completed and playlists can be
	// served.
	Ready bool
}

// Describe returns what discovery has found. It takes the watcher's lock once,
// so the tracks and flags it returns all come from the same moment: a
// rediscovery cannot land between reading the variants and the renditions.
func (w *Watcher) Describe() Description {
	w.mu.Lock()
	defer w.mu.Unlock()
	d := Description{
		Variants:            make([]hlsdesc.Variant, 0, len(w.variants)),
		IndependentSegments: w.independentSegments,
		FMP4:                w.fmp4,
		Ready:               len(w.variants) > 0,
	}
	for _, v := range w.variants {
		if !v.rendition {
			d.Variants = append(d.Variants, v.Variant)
		}
		switch v.kind {
		case renditionAudio:
			d.Renditions = append(d.Renditions, hlsdesc.Rendition{
				RepID:    v.RepID,
				GroupID:  v.groupID,
				Name:     v.name,
				Language: v.language,
				Default:  v.isDefault,
				Channels: v.channels,
			})
		case renditionSubtitle:
			d.Subtitles = append(d.Subtitles, hlsdesc.Subtitle{
				RepID:    v.RepID,
				GroupID:  v.groupID,
				Name:     v.name,
				Language: v.language,
				Default:  v.isDefault,
				Forced:   v.forced,
			})
		}
	}
	return d
}

// PeriodID returns the current period identifier. It changes when the origin
// restarts its media sequence.
func (w *Watcher) PeriodID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return fmt.Sprintf("%d", w.period)
}

// Run discovers the source's variants and then polls each variant's media
// playlist until the context is cancelled or every variant has ended.
//
// It returns nil once all variants report EXT-X-ENDLIST, which the channel
// manager treats the same way it treats a static MPD: ingestion is complete and
// the channel can settle into VOD.
func (w *Watcher) Run(ctx context.Context) error {
	if err := w.discover(ctx); err != nil {
		w.prog.SetSourceError("playlist", err.Error())
		return fmt.Errorf("HLS source discovery: %w", err)
	}

	w.mu.Lock()
	variants := append([]variant(nil), w.variants...)
	w.pollTargets = len(variants)
	w.endedTargets = make(map[string]bool, len(variants))
	w.mu.Unlock()

	var wg sync.WaitGroup
	for _, v := range variants {
		wg.Add(1)
		go func(v variant) {
			defer wg.Done()
			w.pollVariant(ctx, v)
		}(v)
	}
	wg.Wait()

	w.publishVODTotal(len(variants))

	return ctx.Err()
}

// publishVODTotal announces the channel's segment total once every poll target
// has finished, and only when all of them were VOD from their first poll.
//
// A partial sum is deliberately never published: it would be smaller than the
// number of segments already stored across all variants, rendering as a bar past
// 100% with no honest way to display it. A VOD playlist carries EXT-X-ENDLIST on
// the first poll, so in practice this runs before the queued segments have
// finished downloading — which is exactly when the total is wanted.
func (w *Watcher) publishVODTotal(pollTargets int) {
	if pollTargets == 0 {
		return
	}
	w.mu.Lock()
	total := 0
	for _, n := range w.vodVariantTotals {
		total += n
	}
	complete := len(w.vodVariantTotals) == pollTargets
	w.mu.Unlock()

	if complete && total > 0 {
		w.prog.SetExpectedTotal(uint64(total))
	}
}

// PollTargets reports how many playlists Run is currently waiting on.
func (w *Watcher) PollTargets() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pollTargets
}

// OriginIsVOD reports whether every playlist this watcher polls announced
// itself as a finished asset on its first poll.
//
// This is what the channel's mode is promoted on. It is deliberately not
// EndListProgress: that counts variants whose *ingest* has finished, which a
// paced producer does not reach until the asset is queued — far too late to say
// what the channel has been doing all along.
func (w *Watcher) OriginIsVOD() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pollTargets > 0 && len(w.vodVariantTotals) == w.pollTargets
}

// EndListProgress reports how many of the playlists this watcher polls have
// reached EXT-X-ENDLIST, out of how many there are.
//
// It exists because a single variant that never ends — a 404 the poll loop
// retries forever, or a source that simply omits the tag — keeps the whole
// channel out of VOD with nothing to show for it. "3/4 ended" names the one
// that is holding everything up.
func (w *Watcher) EndListProgress() (ended, total int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.endedTargets), w.pollTargets
}

// SetProgress attaches the channel's progress counters. Must be called before Run.
func (w *Watcher) SetProgress(p *progress.Counters) { w.prog = p }

// discover fetches the source URL and works out whether it is a master playlist
// (giving several variants) or a bare media playlist (giving exactly one).
func (w *Watcher) discover(ctx context.Context) error {
	base, err := url.Parse(w.sourceURL)
	if err != nil {
		return fmt.Errorf("parse source URL: %w", err)
	}
	data, err := w.fetch(ctx, w.sourceURL)
	if err != nil {
		return err
	}

	var variants []variant
	independent := false

	if hlssrc.IsMaster(data) {
		master, err := hlssrc.ParseMaster(data, base)
		if err != nil {
			return err
		}
		independent = master.IndependentSegments
		for i, v := range master.Variants {
			variants = append(variants, variant{
				Variant: hlsdesc.Variant{
					RepID:            fmt.Sprintf("v%d", i),
					Bandwidth:        v.Bandwidth,
					AverageBandwidth: v.AverageBandwidth,
					Codecs:           v.Codecs,
					Width:            v.Width,
					Height:           v.Height,
					FrameRate:        v.FrameRate,
					AudioGroup:       v.AudioGroup,
					SubtitleGroup:    v.SubtitleGroup,
				},
				url:       v.URI,
				asID:      hlsASID,
				mediaType: hlsMediaType,
			})
		}
		// Demuxed audio renditions (EXT-X-MEDIA with their own URI) become
		// separate poll targets in the audio AdaptationSet. Renditions without a
		// URI are muxed into the variant and need nothing extra.
		ai, si := 0, 0
		for _, r := range master.Renditions {
			if r.URI == "" {
				continue // muxed into the variant; nothing to poll separately
			}
			switch r.Type {
			case "AUDIO":
				variants = append(variants, variant{
					Variant:   hlsdesc.Variant{RepID: fmt.Sprintf("a%d", ai)},
					url:       r.URI,
					asID:      AudioASID,
					mediaType: index.MediaAudio,
					rendition: true,
					kind:      renditionAudio,
					groupID:   r.GroupID,
					name:      r.Name,
					language:  r.Language,
					isDefault: r.Default,
					channels:  r.Channels,
				})
				ai++
			case "SUBTITLES":
				variants = append(variants, variant{
					Variant:   hlsdesc.Variant{RepID: fmt.Sprintf("s%d", si)},
					url:       r.URI,
					asID:      SubtitleASID,
					mediaType: index.MediaText,
					rendition: true,
					kind:      renditionSubtitle,
					groupID:   r.GroupID,
					name:      r.Name,
					language:  r.Language,
					isDefault: r.Default,
					forced:    r.Forced,
				})
				si++
			}
		}
	} else {
		// A bare media playlist: verify it parses before committing to it, so a
		// wrong URL fails loudly at startup rather than silently never
		// producing segments.
		if _, err := hlssrc.ParseMedia(data, base); err != nil {
			return err
		}
		variants = []variant{{
			Variant:   hlsdesc.Variant{RepID: "v0", Bandwidth: defaultVariantBandwidth},
			url:       w.sourceURL,
			asID:      hlsASID,
			mediaType: hlsMediaType,
		}}
	}

	if len(variants) == 0 {
		return fmt.Errorf("no variants in HLS source %s", w.sourceURL)
	}

	w.mu.Lock()
	w.variants = variants
	w.independentSegments = independent
	w.mu.Unlock()

	w.saveState()

	slog.Info("HLS source discovered",
		"channel", w.channelID,
		"variants", len(variants),
		"url", w.sourceURL,
	)
	return nil
}

// variantState is the per-variant polling state. It lives entirely inside the
// variant's goroutine, so no locking is needed.
type variantState struct {
	// lastSeq is the highest media sequence number already enqueued.
	lastSeq uint64
	// lastMediaSequence is the previous playlist's EXT-X-MEDIA-SEQUENCE, used
	// to detect an origin restart.
	lastMediaSequence uint64
	// ptsAccum is the running presentation time in TSTimescale units.
	ptsAccum int64
	// seen marks that at least one playlist has been processed.
	seen bool
	// period is the period number this state belongs to.
	period int
	// resumed marks state seeded from the index at startup that has not yet
	// seen a playlist. lastMediaSequence is unknown until then, so an origin
	// restart during the gateway's downtime is detected from segment numbers.
	resumed bool
}

// newVariantState builds the initial polling state for a variant, seeding it
// from the segments already in the index for the channel's current period.
//
// This mirrors how the DASH watcher seeds lastKnown from the index on restart:
// without it, the first poll after a restart re-enqueues the entire current
// window (its state thinks it has seen nothing), the processor re-commits
// segments already on disk as duplicate index entries, and the outbound
// playlist — which requires a strictly contiguous run — collapses to a single
// segment. Seeding lastSeq and the PTS accumulator from the highest segment
// already held makes a restart resume cleanly and fetch only genuinely new
// segments. Earlier periods are ignored: their numbering is unrelated to the
// current one's.
func (w *Watcher) newVariantState(v variant) *variantState {
	w.mu.Lock()
	st := &variantState{period: w.period}
	w.mu.Unlock()

	var maxSeg uint64
	var endPTS int64
	found := false

	if rep := w.ci.FindRep(fmt.Sprintf("%d", st.period), v.asID, v.RepID); rep != nil {
		for _, s := range append(rep.Published(), rep.Committed()...) {
			if !found || s.SegNo > maxSeg {
				maxSeg, endPTS, found = s.SegNo, s.EndPTS, true
			}
		}
	}

	if found {
		st.lastSeq = maxSeg
		st.ptsAccum = endPTS
		st.seen = true
		st.resumed = true
	}
	return st
}

// pollVariant reloads one variant's media playlist until the context is
// cancelled or the playlist ends.
func (w *Watcher) pollVariant(ctx context.Context, v variant) {
	st := w.newVariantState(v)
	interval := defaultTargetDuration

	for {
		media, err := w.fetchMedia(ctx, v.url)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("HLS playlist reload failed",
				"channel", w.channelID,
				"rep", v.RepID,
				"url", v.url,
				"err", err,
			)
			w.prog.SetSourceError("playlist", err.Error())
			// RFC 8216 §6.3.4: on failure, retry sooner than the target
			// duration rather than falling further behind the live edge.
			interval = halve(interval)
		} else {
			w.prog.ClearSourceError()
			// Whether this is the variant's first poll decides if its length can
			// be trusted as a total — captured before enqueueNew, which sets seen.
			firstPoll := !st.seen

			// Record a finished playlist's length before queueing it, not after.
			// Both the channel's mode and its segment total are derived from
			// this, and enqueueNew now paces itself against the broker — so it
			// does not return until the whole asset is queued, which for a large
			// one is most of the way through the download. Reading it afterwards
			// meant a VOD ingest reported itself as live, with no total and so no
			// progress bar, for nearly all of the time it was running.
			if firstPoll && media.EndList {
				w.mu.Lock()
				w.vodVariantTotals[v.RepID] = len(media.Segments)
				w.mu.Unlock()
				w.publishVODTotal(w.PollTargets())
			}

			added := w.enqueueNew(ctx, v, st, media)

			if media.TargetDuration > 0 {
				interval = time.Duration(media.TargetDuration) * time.Second
			}
			if added == 0 {
				// The playlist did not change; poll again at half the target
				// duration, as the spec prescribes.
				interval = halve(interval)
			}
			if media.EndList {
				slog.Info("HLS variant ended",
					"channel", w.channelID, "rep", v.RepID)
				w.mu.Lock()
				w.endedTargets[v.RepID] = true
				if firstPoll {
					// Already recorded above for a first-poll VOD playlist; kept
					// for the playlist that only ends on a later poll, whose
					// length is a DVR window and deliberately not a total.
					w.vodVariantTotals[v.RepID] = len(media.Segments)
				}
				w.mu.Unlock()
				return
			}
		}

		if interval < minPollInterval {
			interval = minPollInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// enqueueNew pushes every segment the playlist advertises that has not been
// enqueued yet, and returns how many of them are new at the live edge: what
// the playlist advanced by since the last poll. Holes queued again behind the
// edge do not count, so a playlist that did not change reports zero.
//
// A finished playlist is enumerated exactly once — pollVariant returns as soon
// as it sees EXT-X-ENDLIST — so every segment it names has one chance to be
// queued and the producer must pace itself against the pipeline rather than
// hand the broker more than it holds. A live playlist keeps the old behaviour:
// shedding is how a channel survives an origin it cannot keep up with, and
// the next poll re-advertises whatever is still in the window.
func (w *Watcher) enqueueNew(ctx context.Context, v variant, st *variantState, m *hlssrc.Media) int {
	// An origin restart rewinds the media sequence. Reconciling the new
	// numbering against what is already indexed is not possible in general
	// (segment N after the restart is unrelated to segment N before it), so the
	// channel moves to a fresh period and the variant starts over.
	//
	// After a gateway restart there is no previous media sequence to compare
	// against, so a playlist that ends below the highest segment already held
	// is taken as a restart that happened while the gateway was down. One whose
	// new numbering already passed that mark cannot be told apart and simply
	// continues the current period.
	acrossRestart := st.resumed && len(m.Segments) > 0 &&
		m.Segments[len(m.Segments)-1].SeqNo < st.lastSeq
	if acrossRestart || (st.seen && m.MediaSequence < st.lastMediaSequence) {
		// Across a restart the previous media sequence is unknown; the highest
		// segment held is what the playlist was compared against.
		previous, prevValue := "previous", st.lastMediaSequence
		if acrossRestart {
			previous, prevValue = "highest_held", st.lastSeq
		}
		slog.Warn("upstream media sequence went backwards; treating as a stream reset",
			"channel", w.channelID,
			"rep", v.RepID,
			previous, prevValue,
			"current", m.MediaSequence,
			"across_restart", acrossRestart,
		)
		st.period = w.resetPeriod(st.period)
		st.lastSeq, st.ptsAccum, st.seen = 0, 0, false
	}
	if len(m.Segments) > 0 {
		// An empty playlist gives nothing to compare, so the check waits for
		// the first one that does.
		st.resumed = false
	}
	st.lastMediaSequence = m.MediaSequence

	periodID := fmt.Sprintf("%d", st.period)
	deadline := playlistDeadline(m)

	// Segments already committed for this rep in the current period. Used to
	// tell a genuine hole (still in the origin's window, worth backfilling) from
	// a segment we already hold, mirroring the DASH watcher's knownSegs.
	known := w.knownSpans(v, periodID)

	var advanced, pushed int
	for _, seg := range m.Segments {
		durTicks := int64(math.Round(seg.Duration * TSTimescale))

		var startPTS int64
		backfill := false
		switch {
		case !st.seen || seg.SeqNo > st.lastSeq:
			// A new segment at the live edge: place it on the accumulating
			// timeline and advance.
			if st.seen && seg.SeqNo > st.lastSeq+1 {
				slog.Warn("HLS segments missed, upstream window advanced past the gateway",
					"channel", w.channelID,
					"rep", v.RepID,
					"missed", seg.SeqNo-st.lastSeq-1,
				)
			}
			startPTS = st.ptsAccum
			st.ptsAccum += durTicks
			st.lastSeq = seg.SeqNo
			st.seen = true
			advanced++

		default:
			// seg.SeqNo <= lastSeq: we advanced past this position on an earlier
			// poll. Either we already hold it, or it is a hole that a transient
			// failure left behind and is still inside the origin's window.
			if _, have := known[seg.SeqNo]; have {
				continue // already on disk
			}
			// Backfill the hole, anchoring it right after the previous segment
			// so it lands at its true timeline position rather than the live
			// edge. Without an anchor it cannot be placed, so a later poll (by
			// which time the predecessor has committed) handles it.
			prev, ok := known[seg.SeqNo-1]
			if !ok {
				continue
			}
			startPTS = prev.end
			backfill = true
		}

		task := w.buildTask(v, seg, periodID, startPTS, durTicks, deadline)
		queued := true
		if m.EndList {
			if !w.broker.PushPreserving(ctx, task) {
				return advanced // context cancelled mid-enumeration
			}
		} else {
			// A segment still downloading looks like a hole here too; the
			// broker holds it in flight and does not queue it again.
			queued = w.broker.Push(task)
		}
		if backfill && queued {
			slog.Info("backfilling HLS gap",
				"channel", w.channelID, "rep", v.RepID, "seg", seg.SeqNo)
		}
		pushed++
	}

	if pushed > 0 {
		// Make sure the index knows about the period even before the first
		// segment lands, mirroring what the DASH watcher does.
		w.ci.Period(periodID)
	}
	return advanced
}

// span is a segment's [start, end) presentation range in TSTimescale units.
type span struct{ start, end int64 }

// knownSpans returns the presentation ranges of every segment already held for
// a representation in the given period, keyed by segment number.
func (w *Watcher) knownSpans(v variant, periodID string) map[uint64]span {
	out := map[uint64]span{}
	rep := w.ci.FindRep(periodID, v.asID, v.RepID)
	if rep == nil {
		return out
	}
	for _, s := range append(rep.Published(), rep.Committed()...) {
		out[s.SegNo] = span{start: s.StartPTS, end: s.EndPTS}
	}
	return out
}

// buildTask constructs the queue task for one playlist segment.
func (w *Watcher) buildTask(v variant, seg hlssrc.Segment, periodID string, startPTS, durTicks int64, deadline time.Time) *queue.SegmentTask {
	task := &queue.SegmentTask{
		ChannelID: w.channelID,
		PeriodID:  periodID,
		ASID:      v.asID,
		RepID:     v.RepID,
		MediaType: v.mediaType,
		SegNo:     seg.SeqNo,
		URL:       seg.URI,
		Format:    queue.FormatTS,
		Duration:  uint64(durTicks),
		Timescale: TSTimescale,
		Priority:  segmentPriority(seg.SeqNo),
		Deadline:  deadline,
		HLSFields: queue.HLSFields{
			StartPTS:      startPTS,
			Discontinuity: seg.Discontinuity,
			// HLS timing/discontinuity/key is not fully recoverable from the
			// files on disk, so every HLS segment records a sidecar entry for
			// restart recovery.
			PersistMeta: true,
		},
	}

	// Subtitle renditions carry WebVTT text segments, timed by the playlist like
	// TS. An EXT-X-MAP would make them fMP4-wrapped subtitles, which are handled
	// by the fMP4 branch below instead.
	if v.kind == renditionSubtitle && seg.Map == nil {
		task.Format = queue.FormatWebVTT
	}

	// An EXT-X-MAP means the variant is fMP4, not TS. Those segments carry their
	// own tfdt timing, so the processor validates them as fMP4 and ignores the
	// supplied StartPTS.
	if seg.Map != nil {
		task.Format = queue.FormatFMP4
		task.InitURL = seg.Map.URI
		if br := seg.Map.ByteRange; br != nil {
			task.InitRangeStart = br.Offset
			task.InitRangeEnd = br.Offset + br.Length - 1
		}
		w.markFMP4()
	}
	if br := seg.ByteRange; br != nil {
		task.RangeStart = br.Offset
		task.RangeEnd = br.Offset + br.Length - 1
	}

	// AES-128 protection. EffectiveIV resolves the IV from the tag, or derives
	// it from the media sequence number when the tag omits one.
	if k := seg.Key; k != nil && k.Method == methodAES128 {
		task.Encrypted = true
		task.KeyURI = k.URI
		task.IV = seg.EffectiveIV()
		task.DecryptOnIngest = w.decryptOnIngest
		w.registerKey(k.URI)
	}
	return task
}

// resetPeriod moves a rendition onto a fresh period after an origin
// media-sequence rewind, given the period number it is currently on, and
// returns the period number it should use.
//
// One origin restart rewinds every rendition, but each has its own poll loop
// and notices at a different moment. Only the rendition that notices first
// starts a new period; the ones that follow join it. The test for "am I first?"
// is whether the caller is still on the channel's latest period — no timing
// window is involved. Bumping unconditionally would give video and audio
// separate periods, and the synthesized MPD would then describe two periods
// carrying one track each.
func (w *Watcher) resetPeriod(current int) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if current == w.period {
		w.period++
	}
	return w.period
}

// markFMP4 records that the source turned out to carry fMP4 segments.
func (w *Watcher) markFMP4() {
	w.mu.Lock()
	changed := !w.fmp4
	w.fmp4 = true
	w.mu.Unlock()
	if changed {
		w.saveState()
	}
}

// fetchMedia fetches and parses one media playlist.
func (w *Watcher) fetchMedia(ctx context.Context, playlistURL string) (*hlssrc.Media, error) {
	base, err := url.Parse(playlistURL)
	if err != nil {
		return nil, fmt.Errorf("parse playlist URL: %w", err)
	}
	data, err := w.fetch(ctx, playlistURL)
	if err != nil {
		return nil, err
	}
	return hlssrc.ParseMedia(data, base)
}

// fetch GETs a playlist through the channel's Origin.
func (w *Watcher) fetch(ctx context.Context, rawURL string) ([]byte, error) {
	data, err := w.origin.Get(ctx, w.client, rawURL, fetch.MaxManifestBytes)
	if se := (*fetch.StatusError)(nil); errors.As(err, &se) {
		return nil, fmt.Errorf("upstream playlist status %d for %s", se.Code, rawURL)
	}
	return data, err
}

// halve returns d/2, used to reload sooner when a playlist did not advance.
func halve(d time.Duration) time.Duration {
	return d / 2
}

// segmentPriority maps a media sequence number to a queue priority. Lower is
// more urgent, so older segments are fetched first — they are the ones about to
// fall out of the origin's window.
func segmentPriority(seqNo uint64) int {
	const maxPriority = math.MaxInt32
	if seqNo > maxPriority {
		return maxPriority
	}
	return int(seqNo)
}

// playlistDeadline estimates when the advertised segments will have fallen out
// of the origin's window, after which fetching them is pointless.
func playlistDeadline(m *hlssrc.Media) time.Time {
	if m.EndList {
		// VOD: segments stay available indefinitely.
		return time.Time{}
	}
	var window float64
	for _, s := range m.Segments {
		window += s.Duration
	}
	if window <= 0 {
		window = float64(defaultTargetDuration / time.Second)
	}
	return time.Now().Add(time.Duration(window * float64(time.Second)))
}
