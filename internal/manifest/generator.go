package manifest

import (
	"strings"
	"time"

	gompeg "github.com/unki2aut/go-mpd"
	xsd "github.com/unki2aut/go-xsd-types"

	"github.com/a840817a/sluice/internal/gwurl"
	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

// GeneratorConfig controls how the gateway MPD is produced.
type GeneratorConfig struct {
	// GatewayBaseURL is the public base URL of this gateway, e.g. "https://gw.example.com"
	GatewayBaseURL string
	// ChannelID identifies the channel in the outbound URL paths.
	ChannelID string
	// LicenseURL is the gateway's PlayReady license proxy endpoint.
	LicenseURL string
	// WidevineLicenseURL is the gateway's Widevine license proxy endpoint.
	WidevineLicenseURL string
	// DisablePlayReady omits PlayReady ContentProtection and license URL rewriting.
	DisablePlayReady bool
	// DisableWidevine omits Widevine ContentProtection.
	DisableWidevine bool
	// WindowDepth is the timeShiftBufferDepth for dynamic MPDs.
	WindowDepth time.Duration
	// SafeEdgeBuffer subtracts from live edge to produce the safe edge.
	SafeEdgeBuffer time.Duration
	// FallbackMinimumUpdatePeriod is used for dynamic MPDs when the upstream
	// omits minimumUpdatePeriod or sets it to zero.
	FallbackMinimumUpdatePeriod time.Duration
	// VODMode forces static MPD output with all published segments (no sliding
	// window). Used after a live→VOD transition.
	VODMode bool
	// FinalDuration is the mediaPresentationDuration announced by the upstream
	// static MPD at the moment of transition. When non-zero it takes precedence
	// over the duration computed by scanning the index, giving a more accurate
	// value when the final segments were not fully downloaded.
	FinalDuration time.Duration
	// StaticIngesting indicates the channel is a static source currently being
	// downloaded. The manifest is served as type="dynamic" with only published
	// segments so players keep re-fetching as more segments become available.
	// Automatically cleared once the download completes and the channel enters VOD mode.
	StaticIngesting bool
	// StaticIngestStart is the wall-clock time when static ingestion began.
	// Used as AvailabilityStartTime so clients have a stable time anchor.
	StaticIngestStart time.Time
	// Now is the wall clock used for publishTime. The zero value means
	// time.Now().UTC(), which is what every production caller wants; tests set
	// it so that generating the same input twice yields identical bytes.
	Now time.Time
}

// Generate builds a gateway MPD from the original parsed MPD and the current
// in-memory channel index.
//
// For static MPDs it outputs all PUBLISHED segments with their Gateway URLs.
// For dynamic MPDs it outputs only the sliding window of PUBLISHED segments.
func Generate(
	orig *imdp.ParsedMPD,
	snap index.Snapshot,
	cfg GeneratorConfig,
) ([]byte, error) {
	base := strings.TrimRight(cfg.GatewayBaseURL, "/")
	generatedAt := cfg.Now
	if generatedAt.IsZero() {
		generatedAt = time.Now().UTC()
	}

	profiles := "urn:mpeg:dash:profile:isoff-main:2011"
	if orig.Raw != nil && orig.Raw.Profiles != "" {
		profiles = orig.Raw.Profiles
	}
	m := &gompeg.MPD{
		Profiles:      profiles,
		MinBufferTime: xsdDurationPtr(2 * time.Second),
	}

	mpdType := "static"
	if orig.Type == imdp.PresentationDynamic && !cfg.VODMode {
		mpdType = "dynamic"
		now := xsd.DateTime(generatedAt)
		m.PublishTime = &now
		m.MinimumUpdatePeriod = xsdDurationPtr(dynamicMinimumUpdatePeriod(orig.MinimumUpdatePeriod, cfg.FallbackMinimumUpdatePeriod))
		if cfg.WindowDepth > 0 {
			m.TimeShiftBufferDepth = xsdDurationPtr(cfg.WindowDepth)
		}
		if !orig.AvailabilityStartTime.IsZero() {
			ast := xsd.DateTime(orig.AvailabilityStartTime)
			m.AvailabilityStartTime = &ast
		}
		if orig.Raw != nil && orig.Raw.SuggestedPresentationDelay != nil {
			m.SuggestedPresentationDelay = orig.Raw.SuggestedPresentationDelay
		}
	} else if cfg.StaticIngesting {
		// Static source currently downloading: serve as dynamic so players
		// keep re-fetching the manifest and discover newly published segments.
		mpdType = "dynamic"
		now := xsd.DateTime(generatedAt)
		m.PublishTime = &now
		m.MinimumUpdatePeriod = xsdDurationPtr(dynamicMinimumUpdatePeriod(0, cfg.FallbackMinimumUpdatePeriod))
		// Large shift buffer lets players seek across the full downloaded range.
		m.TimeShiftBufferDepth = xsdDurationPtr(24 * time.Hour)
		ast := xsd.DateTime(cfg.StaticIngestStart.UTC())
		m.AvailabilityStartTime = &ast
	} else {
		// Static or post-transition VOD: set mediaPresentationDuration.
		// For VOD mode, always prefer the index-scanned span (maxEndPTS-minStartPTS)
		// because the upstream static MPD may only reflect the last live window (~30s).
		// FinalDuration is used only as a fallback when the index is empty.
		var dur time.Duration
		switch {
		case cfg.VODMode:
			dur = vodDuration(orig, snap)
			if dur <= 0 && cfg.FinalDuration > 0 {
				dur = cfg.FinalDuration
			}
		default:
			dur = orig.MediaPresentationDuration
		}
		if dur > 0 {
			m.MediaPresentationDuration = xsdDurationPtr(dur)
		}
	}
	m.Type = &mpdType

	// BaseURL points all relative segment paths at the gateway.
	bu := base + "/"
	m.BaseURL = []*gompeg.BaseURL{{Value: bu}}

	xmlns := "urn:mpeg:dash:schema:mpd:2011"
	m.XMLNS = &xmlns

	for _, origPeriod := range orig.Periods {
		period := &gompeg.Period{}
		if origPeriod.ID != "" {
			period.ID = &origPeriod.ID
		}
		if origPeriod.Start >= 0 {
			period.Start = xsdDurationPtr(origPeriod.Start)
		}
		// In VOD mode the upstream static MPD may only reflect the last live
		// window (~30s), so skip its period duration; the segment timeline is authoritative.
		if orig.Type == imdp.PresentationStatic && origPeriod.Duration > 0 && !cfg.VODMode {
			period.Duration = xsdDurationPtr(origPeriod.Duration)
		}

		for _, origAS := range origPeriod.AdaptationSets {
			as, err := buildAdaptationSet(origAS, origPeriod, orig.Type, snap, cfg, base)
			if err != nil {
				return nil, err
			}
			if as == nil {
				continue
			}
			period.AdaptationSets = append(period.AdaptationSets, as)
		}

		if len(period.AdaptationSets) > 0 {
			m.Period = append(m.Period, period)
		}
	}

	encoded, err := m.Encode()
	if err != nil {
		return nil, err
	}
	// go-mpd's MPD struct only has a single XMLNS field.
	// Inject the cenc and mspr namespace declarations so that prefixed child
	// elements (cenc:pssh, mspr:pro) are valid XML.
	encoded = injectNamespaces(encoded)

	// Go's encoding/xml silently drops namespace-prefixed element names
	// (mspr:pro, cenc:pssh, cenc:default_KID) inside Descriptor.
	// Post-process step 1: replace bare <ContentProtection .../> stubs with the
	// full verbatim XML from RawCPs (preserves mspr:pro, cenc:pssh children).
	encoded, err = injectContentProtections(encoded, orig, cfg)
	if err != nil {
		return nil, err
	}

	// Post-process step 2: rewrite LA_URL inside all <pro>...</pro> elements.
	// encoding/xml re-encodes mspr:pro as <pro xmlns="urn:microsoft:playready">.
	if !cfg.DisablePlayReady && cfg.LicenseURL != "" {
		encoded = rewriteProElements(encoded, cfg.LicenseURL)
	}
	return encoded, nil
}

func buildAdaptationSet(
	origAS *imdp.ParsedAdaptationSet,
	origPeriod *imdp.ParsedPeriod,
	origType imdp.PresentationType,
	snap index.Snapshot,
	cfg GeneratorConfig,
	base string,
) (*gompeg.AdaptationSet, error) {
	as := &gompeg.AdaptationSet{
		MimeType: origAS.MimeType,
	}
	if origAS.Lang != "" {
		as.Lang = &origAS.Lang
	}

	// Set ContentProtections so go-mpd emits stubs in the correct positions.
	// The stubs are then replaced with full verbatim XML in injectContentProtections.
	as.ContentProtections = filterContentProtections(origAS.ContentProtections, cfg)

	for _, origRep := range origAS.Representations {
		rep, segTmpl, err := buildRepresentation(origRep, origAS, origPeriod, origType, snap, cfg, base)
		if err != nil {
			return nil, err
		}
		if rep == nil {
			continue
		}
		// Attach SegmentTemplate at rep level
		rep.SegmentTemplate = segTmpl
		as.Representations = append(as.Representations, *rep)
	}

	if len(as.Representations) == 0 {
		return nil, nil
	}
	return as, nil
}

func buildRepresentation(
	origRep *imdp.ParsedRepresentation,
	origAS *imdp.ParsedAdaptationSet,
	origPeriod *imdp.ParsedPeriod,
	origType imdp.PresentationType,
	snap index.Snapshot,
	cfg GeneratorConfig,
	base string,
) (*gompeg.Representation, *gompeg.SegmentTemplate, error) {
	rep := &gompeg.Representation{
		ID:        &origRep.ID,
		Bandwidth: &origRep.Bandwidth,
	}
	if origRep.Width > 0 {
		rep.Width = &origRep.Width
	}
	if origRep.Height > 0 {
		rep.Height = &origRep.Height
	}
	if origRep.Codecs != "" {
		rep.Codecs = &origRep.Codecs
	}

	tmpl := origRep.EffectiveTemplate(origAS)
	if tmpl == nil {
		// SegmentList source: synthesise a SegmentTemplate from the index.
		sl := origRep.EffectiveSegmentList(origAS)
		if sl == nil {
			return rep, nil, nil
		}
		return buildRepFromSegmentList(origRep, origAS, origPeriod, origType, snap, cfg, sl, rep)
	}

	// Build segment template pointing to gateway paths. The URL shape, including
	// why DASH advertises zero-padded segment numbers, lives in gwurl.
	mediaPath := gwurl.SegmentTemplatePath(cfg.ChannelID, origRep.ID)
	initPath := gwurl.InitTemplatePath(cfg.ChannelID, origRep.ID)

	st := &gompeg.SegmentTemplate{
		Media:          &mediaPath,
		Initialization: &initPath,
	}

	// For static MPDs: use fixed duration template
	if tmpl.Timescale > 0 {
		st.Timescale = &tmpl.Timescale
	}
	if tmpl.Duration > 0 {
		st.Duration = &tmpl.Duration
		st.StartNumber = &tmpl.StartNumber
	}
	// Pass through presentationTimeOffset so the player can correctly map segment
	// numbers to wall-clock time (required for live streams with large startNumbers).
	if tmpl.PresentationTimeOffset > 0 {
		pto := tmpl.PresentationTimeOffset
		st.PresentationTimeOffset = &pto
	}

	// For dynamic MPDs with published segments: emit SegmentTimeline.
	// Key by origAS.ID (positional index) to support multiple ASes of the same
	// MediaType. Everything read here comes from the snapshot, so the whole
	// manifest describes one instant and the processor cannot move underneath it.
	//
	// This is deliberately not gated on GatewayBaseURL. An empty base is the
	// documented default (relative URLs, resolved against the manifest's own
	// URL), and a timeline source has no fixed duration to fall back on — so
	// skipping the timeline there emitted a representation carrying no segment
	// info at all, which players reject outright.
	//
	// Absent and empty are the same thing here: either way no timeline is
	// emitted and the representation keeps its fixed-duration template.
	// Segments arrive already sorted by SegNo.
	published := projectPublishedSegments(origType, snap.Published(origPeriod.ID, origAS.ID, origRep.ID), cfg)
	if len(published) > 0 && tmpl.Timescale > 0 {
		timeline := buildTimeline(published)
		if timeline != nil {
			st.SegmentTimeline = timeline
			st.Duration = nil // timeline supersedes fixed duration
			startNo := published[0].SegNo
			st.StartNumber = &startNo
			// Align PresentationTimeOffset with the actual media StartPTS so
			// that players compute (T - PTO) / timescale = seconds from window
			// start, not seconds since epoch (which would show seekbar as "N years").
			if published[0].StartPTS > 0 {
				pto := uint64(published[0].StartPTS)
				st.PresentationTimeOffset = &pto
			}
		}
	}

	return rep, st, nil
}

// buildRepFromSegmentList synthesises a gateway SegmentTemplate for a
// representation whose upstream uses SegmentList. The segments are already
// stored on disk under the normal numbered layout, so the output template
// points to the same gateway paths as SegmentTemplate representations.
func buildRepFromSegmentList(
	origRep *imdp.ParsedRepresentation,
	origAS *imdp.ParsedAdaptationSet,
	origPeriod *imdp.ParsedPeriod,
	origType imdp.PresentationType,
	snap index.Snapshot,
	cfg GeneratorConfig,
	sl *imdp.ParsedSegmentList,
	rep *gompeg.Representation,
) (*gompeg.Representation, *gompeg.SegmentTemplate, error) {
	// Unlike buildRepresentation, a SegmentList representation with nothing
	// published is dropped from the manifest rather than emitted with a fixed
	// duration: there is no upstream template to fall back on.
	published := projectPublishedSegments(origType, snap.Published(origPeriod.ID, origAS.ID, origRep.ID), cfg)
	if len(published) == 0 {
		return nil, nil, nil // not ready yet
	}

	mediaPath := gwurl.SegmentTemplatePath(cfg.ChannelID, origRep.ID)
	initPath := gwurl.InitTemplatePath(cfg.ChannelID, origRep.ID)
	st := &gompeg.SegmentTemplate{
		Media:          &mediaPath,
		Initialization: &initPath,
	}
	startNo := published[0].SegNo
	st.StartNumber = &startNo

	// Prefer timescale from SegmentList; fall back to what we recorded in the index.
	ts := sl.Timescale
	if ts == 0 && len(published) > 0 {
		ts = uint64(published[0].Timescale)
	}
	if ts > 0 {
		st.Timescale = &ts
	}

	if sl.Duration > 0 && ts > 0 {
		d := sl.Duration
		st.Duration = &d
	} else if ts > 0 {
		timeline := buildTimeline(published)
		if timeline != nil {
			st.SegmentTimeline = timeline
		}
	}

	return rep, st, nil
}

// buildTimeline converts a list of published SegmentState entries to a SegmentTimeline,
// collapsing consecutive equal-duration entries with the r (repeat) attribute.
func buildTimeline(segs []index.SegmentState) *gompeg.SegmentTimeline {
	if len(segs) == 0 {
		return nil
	}
	timeline := &gompeg.SegmentTimeline{}
	var cur *gompeg.SegmentTimelineS

	for _, seg := range segs {
		dur := uint64(seg.EndPTS - seg.StartPTS)
		pts := uint64(seg.StartPTS)

		if cur == nil {
			cur = &gompeg.SegmentTimelineS{T: &pts, D: dur}
		} else if cur.D == dur && nextTimelinePTS(cur) == pts {
			// Extend the repeat count
			if cur.R == nil {
				r := int64(1)
				cur.R = &r
			} else {
				*cur.R++
			}
		} else {
			timeline.S = append(timeline.S, cur)
			cur = &gompeg.SegmentTimelineS{T: &pts, D: dur}
		}
	}
	if cur != nil {
		timeline.S = append(timeline.S, cur)
	}
	return timeline
}

func dynamicMinimumUpdatePeriod(orig, fallback time.Duration) time.Duration {
	if orig > 0 {
		return orig
	}
	if fallback > 0 {
		return fallback
	}
	return 2 * time.Second
}

func projectPublishedSegments(origType imdp.PresentationType, segs []index.SegmentState, cfg GeneratorConfig) []index.SegmentState {
	if len(segs) == 0 {
		return nil
	}
	if origType != imdp.PresentationDynamic || cfg.VODMode || cfg.StaticIngesting {
		return segs
	}

	liveEdge := segs[len(segs)-1].EndSec()
	for _, seg := range segs {
		if end := seg.EndSec(); end > liveEdge {
			liveEdge = end
		}
	}

	safeEdge := liveEdge
	if cfg.SafeEdgeBuffer > 0 {
		safeEdge -= cfg.SafeEdgeBuffer.Seconds()
	}
	if safeEdge <= 0 {
		return segs
	}

	windowStart := -1.0
	if cfg.WindowDepth > 0 {
		windowStart = safeEdge - cfg.WindowDepth.Seconds()
	}

	out := make([]index.SegmentState, 0, len(segs))
	for _, seg := range segs {
		end := seg.EndSec()
		if end > safeEdge {
			continue
		}
		if windowStart >= 0 && end <= windowStart {
			continue
		}
		out = append(out, seg)
	}
	if len(out) == 0 {
		return segs
	}
	return out
}

func filterContentProtections(cps []gompeg.Descriptor, cfg GeneratorConfig) []gompeg.Descriptor {
	if !cfg.DisablePlayReady && !cfg.DisableWidevine {
		return cps
	}
	out := make([]gompeg.Descriptor, 0, len(cps))
	for _, cp := range cps {
		sid := ""
		if cp.SchemeIDURI != nil {
			sid = *cp.SchemeIDURI
		}
		if contentProtectionAllowed(sid, cfg) {
			out = append(out, cp)
		}
	}
	return out
}

func contentProtectionAllowed(schemeIDURI string, cfg GeneratorConfig) bool {
	switch imdp.ClassifyScheme(schemeIDURI) {
	case imdp.ContentProtectionPlayReady:
		return !cfg.DisablePlayReady
	case imdp.ContentProtectionWidevine:
		return !cfg.DisableWidevine
	case imdp.ContentProtectionCENC:
		// The common cenc element carries the default_KID both systems key on,
		// so it survives while either system is still enabled. This is an OR on
		// purpose: making it an AND drops the element as soon as one system is
		// disabled, and every remaining encrypted stream stops decrypting.
		return !cfg.DisablePlayReady || !cfg.DisableWidevine
	default:
		// An unrecognised system (FairPlay, say) is passed through untouched
		// rather than silently dropped.
		return true
	}
}

func nextTimelinePTS(s *gompeg.SegmentTimelineS) uint64 {
	if s == nil || s.T == nil {
		return 0
	}
	repeats := uint64(1)
	if s.R != nil && *s.R > 0 {
		repeats += uint64(*s.R)
	}
	return *s.T + repeats*s.D
}

// injectContentProtections replaces bare <ContentProtection .../> stubs emitted
// by go-mpd with the verbatim XML extracted from the original MPD (stored in
// ParsedAdaptationSet.RawCPs). This preserves mspr:pro, cenc:pssh, and
// cenc:default_KID which encoding/xml cannot handle.
//
// Stubs are replaced in document order, one per RawCP entry, so that each
// AdaptationSet's ContentProtection elements receive their own correct PSSH/KID
// (important when video and audio use different keys under the same schemeIdUri).
func injectContentProtections(encoded []byte, orig *imdp.ParsedMPD, cfg GeneratorConfig) ([]byte, error) {
	type sub struct {
		stub string
		raw  string
	}
	var subs []sub

	for _, period := range orig.Periods {
		for _, as := range period.AdaptationSets {
			for _, rawCP := range as.RawCPs {
				sid := attrValue(rawCP, "schemeIdUri")
				if sid == "" {
					continue
				}
				if !contentProtectionAllowed(sid, cfg) {
					continue
				}
				val := attrValue(rawCP, "value")
				raw := rawCP
				if !cfg.DisablePlayReady && cfg.LicenseURL != "" {
					raw = rewriteRawCPLAURL(raw, cfg.LicenseURL)
				}
				stub := `<ContentProtection schemeIdUri="` + sid + `"/>`
				if val != "" {
					stub = `<ContentProtection schemeIdUri="` + sid + `" value="` + val + `"/>`
				}
				subs = append(subs, sub{stub: stub, raw: raw})
			}
		}
	}

	if len(subs) == 0 {
		return encoded, nil
	}

	// Replace each stub exactly once (the next occurrence in document order).
	// This ensures that when video and audio share a schemeIdUri but have
	// different PSSHs, each AdaptationSet's stub is replaced with its own CP.
	s := string(encoded)
	for _, r := range subs {
		idx := strings.Index(s, r.stub)
		if idx < 0 {
			continue
		}
		s = s[:idx] + r.raw + s[idx+len(r.stub):]
	}
	return []byte(s), nil
}

// attrValue extracts an XML attribute value from a raw XML string by simple
// string search. Used only to identify stubs, not for semantic parsing.
func attrValue(xml, attr string) string {
	needle := attr + `="`
	idx := strings.Index(xml, needle)
	if idx < 0 {
		return ""
	}
	rest := xml[idx+len(needle):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// rewriteRawCPLAURL rewrites the LA_URL inside an mspr:pro attribute in a
// raw ContentProtection XML string.
func rewriteRawCPLAURL(rawCP, licenseURL string) string {
	const open = `<mspr:pro>`
	const close = `</mspr:pro>`
	start := strings.Index(rawCP, open)
	if start < 0 {
		return rawCP
	}
	end := strings.Index(rawCP[start:], close)
	if end < 0 {
		return rawCP
	}
	b64 := rawCP[start+len(open) : start+end]
	rewritten, err := RewriteLAURL(b64, licenseURL)
	if err != nil {
		return rawCP
	}
	return rawCP[:start+len(open)] + rewritten + rawCP[start+end:]
}

// rewriteProElements rewrites LA_URL inside every <pro>...</pro> element in the
// encoded MPD bytes. go-mpd renders mspr:pro as:
//
//	<pro xmlns="urn:microsoft:playready">base64blob</pro>
func rewriteProElements(b []byte, licenseURL string) []byte {
	const open = `<pro `
	const closeTag = `</pro>`
	s := string(b)
	var result strings.Builder
	for {
		start := strings.Index(s, open)
		if start < 0 {
			result.WriteString(s)
			break
		}
		// Find the end of the opening tag
		tagEnd := strings.Index(s[start:], ">")
		if tagEnd < 0 {
			result.WriteString(s)
			break
		}
		tagEnd += start + 1 // position after '>'

		// Find closing tag
		end := strings.Index(s[tagEnd:], closeTag)
		if end < 0 {
			result.WriteString(s)
			break
		}
		end += tagEnd

		b64 := s[tagEnd:end]
		rewritten, err := RewriteLAURL(b64, licenseURL)
		if err != nil {
			// keep original on error
			rewritten = b64
		}
		result.WriteString(s[:tagEnd])
		result.WriteString(rewritten)
		s = s[end:]
	}
	return []byte(result.String())
}

// injectNamespaces adds xmlns:cenc and xmlns:mspr declarations to the MPD
// root element so that prefixed child elements are valid XML.
func injectNamespaces(b []byte) []byte {
	const marker = `<MPD `
	const inject = `xmlns:cenc="urn:mpeg:cenc:2013" xmlns:mspr="urn:microsoft:playready" `
	s := string(b)
	if idx := strings.Index(s, marker); idx >= 0 {
		s = s[:idx+len(marker)] + inject + s[idx+len(marker):]
	}
	return []byte(s)
}

// vodDuration computes total content duration from the channel index by
// calculating (maxEndPTS - minStartPTS) / timescale per timescale group and
// returning the longest span. This correctly handles live streams where PTS
// values are large absolute timestamps — using maxEndPTS alone would yield
// decades; the span gives the actual recording length.
func vodDuration(orig *imdp.ParsedMPD, snap index.Snapshot) time.Duration {
	type span struct {
		start, end int64
		set        bool
	}
	spans := make(map[uint32]*span)

	snap.ForEachRep(func(_ index.RepRef, published []index.SegmentState) {
		for _, seg := range published {
			if seg.Timescale == 0 || seg.EndPTS == 0 {
				continue
			}
			sp := spans[seg.Timescale]
			if sp == nil {
				spans[seg.Timescale] = &span{start: seg.StartPTS, end: seg.EndPTS, set: true}
				continue
			}
			if seg.StartPTS < sp.start {
				sp.start = seg.StartPTS
			}
			if seg.EndPTS > sp.end {
				sp.end = seg.EndPTS
			}
		}
	})

	var maxDurSec float64
	for ts, sp := range spans {
		if sp.set && sp.end > sp.start {
			if d := float64(sp.end-sp.start) / float64(ts); d > maxDurSec {
				maxDurSec = d
			}
		}
	}

	if maxDurSec <= 0 {
		return orig.MediaPresentationDuration
	}
	return time.Duration(maxDurSec * float64(time.Second))
}

// xsdDurationPtr converts a time.Duration to a *xsd.Duration.
func xsdDurationPtr(d time.Duration) *xsd.Duration {
	secs := int64(d.Seconds())
	ns := int64(d) - secs*int64(time.Second)
	return &xsd.Duration{Seconds: secs, Nanoseconds: ns}
}
