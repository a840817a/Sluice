// Package mpd handles parsing of upstream MPD files into our internal model.
package mpd

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"math"
	"strings"
	"time"

	gompeg "github.com/unki2aut/go-mpd"
	"github.com/unki2aut/go-xsd-types"
)

// PresentationType distinguishes live (dynamic) from VOD (static) MPDs.
type PresentationType string

const (
	PresentationStatic  PresentationType = "static"
	PresentationDynamic PresentationType = "dynamic"
)

// MediaType classifies an AdaptationSet.
type MediaType string

const (
	MediaVideo   MediaType = "video"
	MediaAudio   MediaType = "audio"
	MediaText    MediaType = "text"
	MediaUnknown MediaType = "unknown"
)

// ParsedMPD is our internal representation of an upstream MPD.
type ParsedMPD struct {
	Type                      PresentationType
	AvailabilityStartTime     time.Time // only meaningful for dynamic
	MinimumUpdatePeriod       time.Duration
	TimeShiftBufferDepth      time.Duration
	MediaPresentationDuration time.Duration // only for static
	BaseURL                   string        // primary <BaseURL> (index 0)
	AltBaseURLs               []string      // fallback <BaseURL> elements (index 1+)

	Periods []*ParsedPeriod

	// Raw preserves the original go-mpd struct for fields we don't otherwise use.
	Raw *gompeg.MPD
}

// ParsedPeriod is one Period within an MPD.
type ParsedPeriod struct {
	ID             string
	Start          time.Duration
	Duration       time.Duration
	BaseURL        string   // primary <BaseURL>
	AltBaseURLs    []string // fallback <BaseURL> elements (index 1+)
	AdaptationSets []*ParsedAdaptationSet
}

// ParsedAdaptationSet is one AdaptationSet.
type ParsedAdaptationSet struct {
	ID                 string
	MediaType          MediaType
	MimeType           string
	Lang               string
	BaseURL            string   // primary <BaseURL>
	AltBaseURLs        []string // fallback <BaseURL> elements (index 1+)
	ContentProtections []gompeg.Descriptor
	// RawCPs holds the verbatim XML string for each ContentProtection element,
	// including namespace-prefixed attributes and children that encoding/xml drops.
	// Indexed in the same order as ContentProtections.
	RawCPs                   []string
	ParsedContentProtections []ParsedContentProtection
	Representations          []*ParsedRepresentation

	// Effective SegmentTemplate (may be inherited from AdaptationSet level)
	SegTemplate *ParsedSegmentTemplate
	// SegList is non-nil when the AdaptationSet uses <SegmentList>.
	SegList *ParsedSegmentList
	// SegBase is non-nil when the AdaptationSet uses <SegmentBase>.
	SegBase *ParsedSegmentBase
}

// ParsedRepresentation is one Representation inside an AdaptationSet.
type ParsedRepresentation struct {
	ID          string
	Bandwidth   uint64
	Width       uint64
	Height      uint64
	Codecs      string
	MimeType    string   // inherits from AdaptationSet if empty
	BaseURL     string   // primary <BaseURL>
	AltBaseURLs []string // fallback <BaseURL> elements (index 1+)

	// Rep-level SegmentTemplate overrides AdaptationSet-level if present.
	SegTemplate *ParsedSegmentTemplate
	// SegList overrides AS-level if present.
	SegList *ParsedSegmentList
	// SegBase overrides AS-level if present.
	SegBase *ParsedSegmentBase
}

// ContentProtectionSystem identifies the DRM system signaled by a
// ContentProtection element.
type ContentProtectionSystem string

const (
	ContentProtectionUnknown   ContentProtectionSystem = "unknown"
	ContentProtectionCENC      ContentProtectionSystem = "cenc"
	ContentProtectionPlayReady ContentProtectionSystem = "playready"
	ContentProtectionWidevine  ContentProtectionSystem = "widevine"
)

// ParsedContentProtection is the structured form of a ContentProtection XML
// element. RawXML is kept so callers can still round-trip the original element
// when namespace-sensitive children such as cenc:pssh are present.
type ParsedContentProtection struct {
	System      ContentProtectionSystem
	SchemeIDURI string
	Value       string
	DefaultKID  string
	PSSHs       []string
	MSPRPros    []string
	RawXML      string
}

// ParsedSegmentTemplate holds the info needed to compute segment URLs and numbers.
type ParsedSegmentTemplate struct {
	Timescale              uint64
	Duration               uint64 // per-segment duration in timescale units; 0 if using SegmentTimeline
	StartNumber            uint64
	PresentationTimeOffset uint64 // maps segment numbers to media time; 0 if not set
	Media                  string // e.g. "manifest_6m_$Number%09d$.mp4"
	Init                   string // e.g. "manifest_6minit.mp4"

	// SegmentTimeline entries (nil for number-based templates)
	Timeline []SegmentTimelineEntry
}

// SegmentTimelineEntry represents one <S> element.
type SegmentTimelineEntry struct {
	T uint64 // presentation time
	D uint64 // duration
	R int64  // repeat count (-1 = infinite)
}

// ParsedSegmentList describes an explicit segment list (<SegmentList>).
// Segments are numbered from StartNumber (default 1).
type ParsedSegmentList struct {
	Timescale   uint64
	Duration    uint64 // per-segment duration in timescale units (0 = not specified)
	StartNumber uint64 // first segment number (default 1)
	InitURL     string // from <Initialization sourceURL="...">; empty = none
	InitRange   string // byte range ("start-end") if init is a range within BaseURL
	Segments    []SegmentListEntry
}

// SegmentListEntry is one <SegmentURL> inside a SegmentList.
type SegmentListEntry struct {
	URL        string // from media="..."; may be empty (use BaseURL)
	MediaRange string // byte range ("start-end"); non-empty only for byte-range access
}

// ParsedSegmentBase describes a single-file representation (<SegmentBase>).
// The actual file URL comes from the nearest <BaseURL> element in scope.
type ParsedSegmentBase struct {
	Timescale  uint32
	IndexRange string // byte range of the sidx box, e.g. "0-836"
	InitRange  string // byte range of the init (moov) segment, e.g. "0-698"
}

// SegmentDuration returns the duration of one segment in seconds.
// Uses the fixed Duration field for number-based templates.
// For timeline-based templates returns 0 (caller must compute per-entry).
func (t *ParsedSegmentTemplate) SegmentDuration() time.Duration {
	if t.Duration == 0 || t.Timescale == 0 {
		return 0
	}
	return time.Duration(float64(t.Duration) / float64(t.Timescale) * float64(time.Second))
}

// EffectiveTemplate returns a merged SegmentTemplate: rep-level fields take
// precedence, but any zero-value fields fall back to the AdaptationSet level.
// This handles the common pattern where timescale sits on the AdaptationSet
// but media/initialization sit on each Representation.
func (r *ParsedRepresentation) EffectiveTemplate(parent *ParsedAdaptationSet) *ParsedSegmentTemplate {
	if r.SegTemplate == nil {
		return parent.SegTemplate
	}
	if parent.SegTemplate == nil {
		return r.SegTemplate
	}
	// Merge: start with a copy of the rep-level template then fill zeros from parent.
	merged := *r.SegTemplate
	if merged.Timescale == 0 {
		merged.Timescale = parent.SegTemplate.Timescale
	}
	if merged.Duration == 0 {
		merged.Duration = parent.SegTemplate.Duration
	}
	if merged.StartNumber == 0 {
		merged.StartNumber = parent.SegTemplate.StartNumber
	}
	if merged.Media == "" {
		merged.Media = parent.SegTemplate.Media
	}
	if merged.Init == "" {
		merged.Init = parent.SegTemplate.Init
	}
	if len(merged.Timeline) == 0 {
		merged.Timeline = parent.SegTemplate.Timeline
	}
	return &merged
}

// EffectiveSegmentList returns the rep-level SegmentList if present,
// otherwise the AS-level one.
func (r *ParsedRepresentation) EffectiveSegmentList(parent *ParsedAdaptationSet) *ParsedSegmentList {
	if r.SegList != nil {
		return r.SegList
	}
	return parent.SegList
}

// EffectiveSegmentBase returns the rep-level SegmentBase if present,
// otherwise the AS-level one.
func (r *ParsedRepresentation) EffectiveSegmentBase(parent *ParsedAdaptationSet) *ParsedSegmentBase {
	if r.SegBase != nil {
		return r.SegBase
	}
	return parent.SegBase
}

// EffectiveBaseURL returns the nearest non-empty BaseURL in the rep→AS chain.
func (r *ParsedRepresentation) EffectiveBaseURL(parent *ParsedAdaptationSet) string {
	if r.BaseURL != "" {
		return r.BaseURL
	}
	return parent.BaseURL
}

// LastSegmentNumber computes the expected last segment number for a static MPD
// given the period duration. It returns 0 when the period holds no segments.
func (t *ParsedSegmentTemplate) LastSegmentNumber(periodDuration time.Duration) uint64 {
	if t.Duration == 0 || t.Timescale == 0 || periodDuration <= 0 {
		return 0
	}
	totalTicks := uint64(periodDuration.Seconds() * float64(t.Timescale))
	count := uint64(math.Ceil(float64(totalTicks) / float64(t.Duration)))
	if count == 0 {
		// A period shorter than a single tick rounds to no segments. Falling
		// through would compute StartNumber-1, which wraps to MaxUint64 for
		// startNumber="0" and hands discovery an unbounded segment range.
		return 0
	}
	return t.StartNumber + count - 1
}

// SegmentURL expands the media template for a given segment number and
// presentation time. Handles $Number$, $Number%09d$, $Time$, $Time%09d$,
// and $RepresentationID$ substitutions.
//
// segTime is used for $Time$ and is the segment's presentation start time in
// timescale units (from the SegmentTimeline @t value). For number-based
// templates (no SegmentTimeline) segTime should be 0 as $Time$ is not valid.
func (t *ParsedSegmentTemplate) SegmentURL(segNo, segTime uint64, repID string) string {
	return expandTemplate(t.Media, segNo, segTime, repID)
}

// InitURL expands the initialization template.
func (t *ParsedSegmentTemplate) InitURL(repID string) string {
	return expandTemplate(t.Init, 0, 0, repID)
}

func expandTemplate(tmpl string, segNo, segTime uint64, repID string) string {
	s := strings.ReplaceAll(tmpl, "$RepresentationID$", repID)
	s = expandVarUint(s, "Number", segNo)
	s = expandVarUint(s, "Time", segTime)
	return s
}

// expandVarUint replaces the first occurrence of "$Name$" or "$Name%fmt$"
// in s with the formatted value of val.
func expandVarUint(s, name string, val uint64) string {
	prefix := "$" + name
	i := strings.Index(s, prefix)
	if i < 0 {
		return s
	}
	// rest starts immediately after "$Name"
	rest := s[i+len(prefix):]
	if len(rest) == 0 {
		return s
	}
	end := strings.Index(rest, "$")
	if end < 0 {
		return s
	}
	fmtStr := rest[:end] // "" → plain decimal; "%09d" → padded
	var formatted string
	if fmtStr == "" {
		formatted = fmt.Sprintf("%d", val)
	} else {
		formatted = fmt.Sprintf(fmtStr, val)
	}
	return s[:i] + formatted + rest[end+1:]
}

// ────────────────────────────────────────────────────────────────────────────
// Raw XML helpers for SegmentList / SegmentBase
// (go-mpd does not expose these elements, so we parse them ourselves)
// ────────────────────────────────────────────────────────────────────────────

// extMPD mirrors the MPD XML structure just enough to extract SegmentList,
// SegmentBase, and Representation-level BaseURL values that go-mpd ignores.
type extMPD struct {
	Periods []extPeriod `xml:"Period"`
}

type extPeriod struct {
	AdaptationSets []extAS `xml:"AdaptationSet"`
}

type extAS struct {
	SegList *extSegmentList `xml:"SegmentList"`
	SegBase *extSegmentBase `xml:"SegmentBase"`
	Reps    []extRep        `xml:"Representation"`
}

type extRep struct {
	SegList *extSegmentList `xml:"SegmentList"`
	SegBase *extSegmentBase `xml:"SegmentBase"`
}

type extSegmentList struct {
	Timescale   uint64      `xml:"timescale,attr"`
	Duration    uint64      `xml:"duration,attr"`
	StartNumber uint64      `xml:"startNumber,attr"`
	Init        *extInit    `xml:"Initialization"`
	Segments    []extSegURL `xml:"SegmentURL"`
}

type extSegmentBase struct {
	Timescale  uint32   `xml:"timescale,attr"`
	IndexRange string   `xml:"indexRange,attr"`
	Init       *extInit `xml:"Initialization"`
}

type extInit struct {
	SourceURL string `xml:"sourceURL,attr"`
	Range     string `xml:"range,attr"`
}

type extSegURL struct {
	Media      string `xml:"media,attr"`
	MediaRange string `xml:"mediaRange,attr"`
}

func parseExtMPD(data []byte) extMPD {
	var ext extMPD
	// Best-effort — failures are silently ignored; callers treat nil as absent.
	_ = xml.Unmarshal(data, &ext)
	return ext
}

func convertSegmentList(e *extSegmentList) *ParsedSegmentList {
	if e == nil {
		return nil
	}
	sl := &ParsedSegmentList{
		Timescale:   e.Timescale,
		Duration:    e.Duration,
		StartNumber: e.StartNumber,
	}
	if sl.StartNumber == 0 {
		sl.StartNumber = 1
	}
	if e.Init != nil {
		sl.InitURL = e.Init.SourceURL
		sl.InitRange = e.Init.Range
	}
	for _, s := range e.Segments {
		sl.Segments = append(sl.Segments, SegmentListEntry{
			URL:        s.Media,
			MediaRange: s.MediaRange,
		})
	}
	return sl
}

func convertSegmentBase(e *extSegmentBase) *ParsedSegmentBase {
	if e == nil {
		return nil
	}
	sb := &ParsedSegmentBase{
		Timescale:  e.Timescale,
		IndexRange: e.IndexRange,
	}
	if e.Init != nil {
		sb.InitRange = e.Init.Range
	}
	return sb
}

// ────────────────────────────────────────────────────────────────────────────
// Parse
// ────────────────────────────────────────────────────────────────────────────

// Parse decodes raw MPD bytes and returns our internal ParsedMPD.
func Parse(data []byte) (*ParsedMPD, error) {
	raw := &gompeg.MPD{}
	if err := raw.Decode(data); err != nil {
		return nil, fmt.Errorf("mpd decode: %w", err)
	}

	// Extract raw ContentProtection XML strings before go-mpd drops the
	// namespace-prefixed children (mspr:pro, cenc:pssh, cenc:default_KID).
	rawCPs := extractRawContentProtections(data)

	// Extract SegmentList / SegmentBase (not supported by go-mpd).
	ext := parseExtMPD(data)

	p := &ParsedMPD{Raw: raw}

	// Presentation type
	if raw.Type != nil && *raw.Type == "dynamic" {
		p.Type = PresentationDynamic
	} else {
		p.Type = PresentationStatic
	}

	// Timings
	if raw.AvailabilityStartTime != nil {
		p.AvailabilityStartTime = xsdDateTimeToTime(raw.AvailabilityStartTime)
	}
	if raw.MinimumUpdatePeriod != nil {
		p.MinimumUpdatePeriod = xsdDurationToGo(raw.MinimumUpdatePeriod)
	}
	if raw.TimeShiftBufferDepth != nil {
		p.TimeShiftBufferDepth = xsdDurationToGo(raw.TimeShiftBufferDepth)
	}
	if raw.MediaPresentationDuration != nil {
		p.MediaPresentationDuration = xsdDurationToGo(raw.MediaPresentationDuration)
	}

	// MPD-level BaseURL (primary + alternates for multi-CDN fallback)
	if len(raw.BaseURL) > 0 {
		p.BaseURL = raw.BaseURL[0].Value
		for _, bu := range raw.BaseURL[1:] {
			p.AltBaseURLs = append(p.AltBaseURLs, bu.Value)
		}
	}

	// Periods
	asIdx := 0
	for pi, rp := range raw.Period {
		var extPeriod *extPeriod
		if pi < len(ext.Periods) {
			extPeriod = &ext.Periods[pi]
		}
		pp, err := parsePeriod(rp, rawCPs, &asIdx, extPeriod)
		if err != nil {
			return nil, err
		}
		p.Periods = append(p.Periods, pp)
	}

	return p, nil
}

func parsePeriod(rp *gompeg.Period, rawCPs [][]string, asIdx *int, ext *extPeriod) (*ParsedPeriod, error) {
	pp := &ParsedPeriod{}
	if rp.ID != nil {
		pp.ID = *rp.ID
	}
	if rp.Start != nil {
		pp.Start = xsdDurationToGo(rp.Start)
	}
	if rp.Duration != nil {
		pp.Duration = xsdDurationToGo(rp.Duration)
	}
	if len(rp.BaseURL) > 0 {
		pp.BaseURL = rp.BaseURL[0].Value
		for _, bu := range rp.BaseURL[1:] {
			pp.AltBaseURLs = append(pp.AltBaseURLs, bu.Value)
		}
	}

	for i, as := range rp.AdaptationSets {
		var myCPs []string
		if *asIdx < len(rawCPs) {
			myCPs = rawCPs[*asIdx]
		}
		*asIdx++
		var extAS *extAS
		if ext != nil && i < len(ext.AdaptationSets) {
			extAS = &ext.AdaptationSets[i]
		}
		pas, err := parseAdaptationSet(as, i, myCPs, extAS)
		if err != nil {
			return nil, err
		}
		pp.AdaptationSets = append(pp.AdaptationSets, pas)
	}
	return pp, nil
}

func parseAdaptationSet(as *gompeg.AdaptationSet, idx int, rawCPs []string, ext *extAS) (*ParsedAdaptationSet, error) {
	pas := &ParsedAdaptationSet{
		ID:                       fmt.Sprintf("%d", idx),
		MimeType:                 as.MimeType,
		ContentProtections:       as.ContentProtections,
		RawCPs:                   rawCPs,
		ParsedContentProtections: parseRawContentProtections(rawCPs),
	}
	if as.Lang != nil {
		pas.Lang = *as.Lang
	}
	pas.MediaType = inferMediaType(as.MimeType, as.ContentType)
	if len(as.BaseURL) > 0 {
		pas.BaseURL = as.BaseURL[0].Value
		for _, bu := range as.BaseURL[1:] {
			pas.AltBaseURLs = append(pas.AltBaseURLs, bu.Value)
		}
	}
	if as.SegmentTemplate != nil {
		pas.SegTemplate = parseSegmentTemplate(as.SegmentTemplate)
	}
	if ext != nil {
		pas.SegList = convertSegmentList(ext.SegList)
		pas.SegBase = convertSegmentBase(ext.SegBase)
	}

	for i, rep := range as.Representations {
		var extRep *extRep
		if ext != nil && i < len(ext.Reps) {
			extRep = &ext.Reps[i]
		}
		pr := parseRepresentation(&rep, as.MimeType, extRep)
		pas.Representations = append(pas.Representations, pr)
	}
	return pas, nil
}

func parseRepresentation(r *gompeg.Representation, parentMime string, ext *extRep) *ParsedRepresentation {
	pr := &ParsedRepresentation{MimeType: parentMime}
	if r.ID != nil {
		pr.ID = *r.ID
	}
	if r.Bandwidth != nil {
		pr.Bandwidth = *r.Bandwidth
	}
	if r.Width != nil {
		pr.Width = *r.Width
	}
	if r.Height != nil {
		pr.Height = *r.Height
	}
	if r.Codecs != nil {
		pr.Codecs = *r.Codecs
	}
	if r.SegmentTemplate != nil {
		pr.SegTemplate = parseSegmentTemplate(r.SegmentTemplate)
	}
	if len(r.BaseURL) > 0 {
		pr.BaseURL = r.BaseURL[0].Value
		for _, bu := range r.BaseURL[1:] {
			pr.AltBaseURLs = append(pr.AltBaseURLs, bu.Value)
		}
	}
	if ext != nil {
		pr.SegList = convertSegmentList(ext.SegList)
		pr.SegBase = convertSegmentBase(ext.SegBase)
	}
	return pr
}

func parseSegmentTemplate(st *gompeg.SegmentTemplate) *ParsedSegmentTemplate {
	pst := &ParsedSegmentTemplate{}
	if st.Timescale != nil {
		pst.Timescale = *st.Timescale
	}
	if st.Duration != nil {
		pst.Duration = *st.Duration
	}
	if st.StartNumber != nil {
		pst.StartNumber = *st.StartNumber
	} else {
		pst.StartNumber = 1
	}
	if st.PresentationTimeOffset != nil {
		pst.PresentationTimeOffset = *st.PresentationTimeOffset
	}
	if st.Media != nil {
		pst.Media = *st.Media
	}
	if st.Initialization != nil {
		pst.Init = *st.Initialization
	}
	if st.SegmentTimeline != nil {
		for _, s := range st.SegmentTimeline.S {
			entry := SegmentTimelineEntry{D: s.D}
			if s.T != nil {
				entry.T = *s.T
			}
			if s.R != nil {
				entry.R = *s.R
			}
			pst.Timeline = append(pst.Timeline, entry)
		}
	}
	return pst
}

func xsdDurationToGo(d *xsd.Duration) time.Duration {
	if d == nil {
		return 0
	}
	ns, err := d.ToNanoseconds()
	if err != nil {
		return 0
	}
	return time.Duration(ns)
}

func xsdDateTimeToTime(dt *xsd.DateTime) time.Time {
	if dt == nil {
		return time.Time{}
	}
	return time.Time(*dt)
}

func inferMediaType(mimeType string, contentType *string) MediaType {
	if contentType != nil {
		switch *contentType {
		case "video":
			return MediaVideo
		case "audio":
			return MediaAudio
		case "text":
			return MediaText
		}
	}
	switch {
	case strings.HasPrefix(mimeType, "video/"):
		return MediaVideo
	case strings.HasPrefix(mimeType, "audio/"):
		return MediaAudio
	case strings.HasPrefix(mimeType, "text/"):
		return MediaText
	}
	return MediaUnknown
}

// extractRawContentProtections parses the raw MPD bytes using a streaming XML
// decoder and returns one []string per AdaptationSet, where each string is the
// verbatim XML of one ContentProtection element (including namespace-prefixed
// children that encoding/xml would otherwise drop).
//
// Outer slice index = AdaptationSet order in document.
// Inner slice index = ContentProtection order within that AdaptationSet.
func extractRawContentProtections(data []byte) [][]string {
	var result [][]string
	var current []string
	inAS := false

	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "AdaptationSet":
				inAS = true
				current = nil
			case "ContentProtection":
				if inAS {
					raw, err := tokenToString(t, dec)
					if err == nil {
						current = append(current, raw)
					}
				}
			}
		case xml.EndElement:
			if t.Name.Local == "AdaptationSet" {
				result = append(result, current)
				inAS = false
				current = nil
			}
		}
	}
	return result
}

func parseRawContentProtections(rawCPs []string) []ParsedContentProtection {
	out := make([]ParsedContentProtection, 0, len(rawCPs))
	for _, raw := range rawCPs {
		cp := parseRawContentProtection(raw)
		if cp.SchemeIDURI == "" && cp.RawXML == "" {
			continue
		}
		out = append(out, cp)
	}
	return out
}

func parseRawContentProtection(raw string) ParsedContentProtection {
	cp := ParsedContentProtection{
		System: ContentProtectionUnknown,
		RawXML: raw,
	}

	dec := xml.NewDecoder(strings.NewReader(raw))
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "ContentProtection":
				for _, attr := range t.Attr {
					switch attr.Name.Local {
					case "schemeIdUri":
						cp.SchemeIDURI = attr.Value
					case "value":
						cp.Value = attr.Value
					case "default_KID":
						cp.DefaultKID = attr.Value
					}
				}
				cp.System = ClassifyScheme(cp.SchemeIDURI)
			case "pssh":
				if text, ok := readElementText(dec, t.Name.Local); ok {
					cp.PSSHs = append(cp.PSSHs, strings.TrimSpace(text))
				}
			case "pro":
				if text, ok := readElementText(dec, t.Name.Local); ok {
					cp.MSPRPros = append(cp.MSPRPros, strings.TrimSpace(text))
				}
			}
		}
	}

	return cp
}

// ClassifyScheme maps a ContentProtection schemeIdUri to the DRM system it
// signals, returning ContentProtectionUnknown for anything unrecognised.
//
// It is exported because the manifest generator must make the same
// classification when deciding which ContentProtection elements to emit, and
// two copies of these UUIDs would be a silent DRM failure waiting to happen.
func ClassifyScheme(schemeIDURI string) ContentProtectionSystem {
	sid := strings.ToLower(schemeIDURI)
	switch {
	case strings.Contains(sid, "9a04f079-9840-4286-ab92-e65be0885f95") ||
		strings.Contains(sid, "microsoft:playready"):
		return ContentProtectionPlayReady
	case strings.Contains(sid, "edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"):
		return ContentProtectionWidevine
	case strings.Contains(sid, "mp4protection"):
		return ContentProtectionCENC
	default:
		return ContentProtectionUnknown
	}
}

func readElementText(dec *xml.Decoder, _ string) (string, bool) {
	var b strings.Builder
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return b.String(), false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			b.Write([]byte(t))
		}
	}
	return b.String(), true
}

// tokenToString re-encodes a StartElement and its contents back to an XML string.
func tokenToString(start xml.StartElement, dec *xml.Decoder) (string, error) {
	var buf bytes.Buffer
	enc := xml.NewEncoder(&buf)

	if err := enc.EncodeToken(start); err != nil {
		return "", err
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			_ = enc.EncodeToken(t)
		case xml.EndElement:
			depth--
			_ = enc.EncodeToken(t)
		case xml.CharData:
			_ = enc.EncodeToken(t)
		}
	}
	_ = enc.Flush()
	return buf.String(), nil
}
