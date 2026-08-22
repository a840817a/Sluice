// Package hlssrc parses upstream HLS master and media playlists into an
// internal model. It is deliberately dependency-free: the playlist grammar
// (RFC 8216) is small enough that a hand-rolled parser is easier to audit than
// a third-party one, and we need exact control over URI resolution and over
// which unknown tags are tolerated.
package hlssrc

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Tag names we act on. Anything else is ignored rather than treated as an error,
// so a source that adds tags we do not model keeps working.
const (
	tagM3U                   = "#EXTM3U"
	tagVersion               = "#EXT-X-VERSION"
	tagIndependentSegments   = "#EXT-X-INDEPENDENT-SEGMENTS"
	tagStreamInf             = "#EXT-X-STREAM-INF"
	tagMedia                 = "#EXT-X-MEDIA"
	tagTargetDuration        = "#EXT-X-TARGETDURATION"
	tagMediaSequence         = "#EXT-X-MEDIA-SEQUENCE"
	tagDiscontinuitySequence = "#EXT-X-DISCONTINUITY-SEQUENCE"
	tagPlaylistType          = "#EXT-X-PLAYLIST-TYPE"
	tagInf                   = "#EXTINF"
	tagByteRange             = "#EXT-X-BYTERANGE"
	tagKey                   = "#EXT-X-KEY"
	tagMap                   = "#EXT-X-MAP"
	tagDiscontinuity         = "#EXT-X-DISCONTINUITY"
	tagProgramDateTime       = "#EXT-X-PROGRAM-DATE-TIME"
	tagEndList               = "#EXT-X-ENDLIST"
)

// Master is a parsed multivariant (master) playlist.
type Master struct {
	Version             int
	IndependentSegments bool
	Variants            []Variant
	Renditions          []Rendition
}

// Variant is one #EXT-X-STREAM-INF entry. URI is resolved to absolute.
type Variant struct {
	URI              string
	Bandwidth        uint64
	AverageBandwidth uint64
	Codecs           string
	Width            int
	Height           int
	FrameRate        float64
	AudioGroup       string
	SubtitleGroup    string
	VideoGroup       string
}

// Rendition is one #EXT-X-MEDIA entry. URI is resolved to absolute and may be
// empty (renditions muxed into the variant carry no URI).
type Rendition struct {
	Type       string // AUDIO / VIDEO / SUBTITLES / CLOSED-CAPTIONS
	GroupID    string
	Name       string
	Language   string
	Default    bool
	AutoSelect bool
	Forced     bool
	Channels   string
	URI        string
}

// Media is a parsed media playlist.
type Media struct {
	Version               int
	TargetDuration        int
	MediaSequence         uint64
	DiscontinuitySequence uint64
	PlaylistType          string // VOD / EVENT / "" (live)
	EndList               bool
	Segments              []Segment
}

// Segment is one media segment. URI is resolved to absolute and SeqNo is the
// absolute media sequence number (MediaSequence + index), which is the identity
// every downstream component keys off.
type Segment struct {
	URI             string
	Duration        float64 // EXTINF seconds
	Title           string
	SeqNo           uint64
	Discontinuity   bool
	Key             *Key // nil when the segment is not encrypted
	Map             *MapInfo
	ByteRange       *ByteRange
	ProgramDateTime time.Time
}

// Key is a resolved #EXT-X-KEY. A METHOD=NONE tag clears the running key
// rather than producing one, so a non-nil Key always means encrypted.
type Key struct {
	Method            string // AES-128 / SAMPLE-AES
	URI               string
	IV                []byte // empty when the tag omitted IV
	KeyFormat         string
	KeyFormatVersions string
}

// MapInfo is a resolved #EXT-X-MAP (fMP4 initialization section).
type MapInfo struct {
	URI       string
	ByteRange *ByteRange
}

// ByteRange is a resolved #EXT-X-BYTERANGE.
type ByteRange struct {
	Length uint64
	Offset uint64
}

// EffectiveIV returns the AES-128 initialization vector for this segment.
// When the EXT-X-KEY carried an explicit IV that value is used; otherwise the
// IV is the segment's media sequence number as a 128-bit big-endian integer
// (RFC 8216 §5.2). Returns nil when the segment is not encrypted.
func (s Segment) EffectiveIV() []byte {
	if s.Key == nil {
		return nil
	}
	if len(s.Key.IV) > 0 {
		iv := make([]byte, len(s.Key.IV))
		copy(iv, s.Key.IV)
		return iv
	}
	iv := make([]byte, 16)
	binary.BigEndian.PutUint64(iv[8:], s.SeqNo)
	return iv
}

// IsMaster reports whether data looks like a multivariant playlist.
//
// The check requires #EXT-X-STREAM-INF to appear as a real tag (at the start of
// a line and followed by ':'), not merely as a substring. A naive substring
// test misfires on media playlists whose segment URIs happen to contain the tag
// name.
func IsMaster(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		if hasTag(strings.TrimSpace(line), tagStreamInf) {
			return true
		}
	}
	return false
}

// hasTag reports whether line is exactly tag or tag followed by ':'.
func hasTag(line, tag string) bool {
	if !strings.HasPrefix(line, tag) {
		return false
	}
	rest := line[len(tag):]
	return rest == "" || rest[0] == ':'
}

// tagValue returns the text after "tag:" for a line known to carry that tag.
func tagValue(line, tag string) string {
	rest := line[len(tag):]
	return strings.TrimPrefix(rest, ":")
}

// ParseMaster parses a multivariant playlist. All URIs are resolved against
// base, which must be the absolute URL the playlist itself was fetched from.
func ParseMaster(data []byte, base *url.URL) (*Master, error) {
	lines, err := splitPlaylist(data)
	if err != nil {
		return nil, err
	}

	m := &Master{}
	var pending *Variant

	for _, line := range lines {
		switch {
		case hasTag(line, tagVersion):
			m.Version, _ = strconv.Atoi(tagValue(line, tagVersion))

		case hasTag(line, tagIndependentSegments):
			m.IndependentSegments = true

		case hasTag(line, tagMedia):
			attrs := parseAttributes(tagValue(line, tagMedia))
			r := Rendition{
				Type:       attrs["TYPE"],
				GroupID:    attrs["GROUP-ID"],
				Name:       attrs["NAME"],
				Language:   attrs["LANGUAGE"],
				Default:    attrs["DEFAULT"] == "YES",
				AutoSelect: attrs["AUTOSELECT"] == "YES",
				Forced:     attrs["FORCED"] == "YES",
				Channels:   attrs["CHANNELS"],
			}
			if u, ok := attrs["URI"]; ok && u != "" {
				r.URI = resolveURI(base, u)
			}
			m.Renditions = append(m.Renditions, r)

		case hasTag(line, tagStreamInf):
			attrs := parseAttributes(tagValue(line, tagStreamInf))
			v := Variant{
				Codecs:        attrs["CODECS"],
				AudioGroup:    attrs["AUDIO"],
				SubtitleGroup: attrs["SUBTITLES"],
				VideoGroup:    attrs["VIDEO"],
			}
			v.Bandwidth, _ = strconv.ParseUint(attrs["BANDWIDTH"], 10, 64)
			v.AverageBandwidth, _ = strconv.ParseUint(attrs["AVERAGE-BANDWIDTH"], 10, 64)
			v.FrameRate, _ = strconv.ParseFloat(attrs["FRAME-RATE"], 64)
			v.Width, v.Height = parseResolution(attrs["RESOLUTION"])
			pending = &v

		case strings.HasPrefix(line, "#"):
			// Unknown tag or comment: ignore.

		default:
			// A URI line completes the preceding EXT-X-STREAM-INF.
			if pending != nil {
				pending.URI = resolveURI(base, line)
				m.Variants = append(m.Variants, *pending)
				pending = nil
			}
		}
	}

	if len(m.Variants) == 0 {
		return nil, errors.New("master playlist contains no variants")
	}
	return m, nil
}

// ParseMedia parses a media playlist. All URIs are resolved against base, which
// must be the absolute URL the playlist itself was fetched from.
func ParseMedia(data []byte, base *url.URL) (*Media, error) {
	lines, err := splitPlaylist(data)
	if err != nil {
		return nil, err
	}

	m := &Media{}

	// State carried across segments: EXT-X-KEY and EXT-X-MAP apply to every
	// following segment until overridden, and an offset-less EXT-X-BYTERANGE
	// continues from the end of the previous range for the same URI.
	var (
		curKey          *Key
		curMap          *MapInfo
		pendingDur      float64
		pendingTitle    string
		pendingRange    *ByteRange
		pendingDiscont  bool
		pendingPDT      time.Time
		haveInf         bool
		lastRangeEndFor = map[string]uint64{}
	)

	for _, line := range lines {
		switch {
		case hasTag(line, tagVersion):
			m.Version, _ = strconv.Atoi(tagValue(line, tagVersion))

		case hasTag(line, tagTargetDuration):
			m.TargetDuration, _ = strconv.Atoi(tagValue(line, tagTargetDuration))

		case hasTag(line, tagMediaSequence):
			m.MediaSequence, _ = strconv.ParseUint(strings.TrimSpace(tagValue(line, tagMediaSequence)), 10, 64)

		case hasTag(line, tagDiscontinuitySequence):
			m.DiscontinuitySequence, _ = strconv.ParseUint(strings.TrimSpace(tagValue(line, tagDiscontinuitySequence)), 10, 64)

		case hasTag(line, tagPlaylistType):
			m.PlaylistType = strings.TrimSpace(tagValue(line, tagPlaylistType))

		case hasTag(line, tagEndList):
			m.EndList = true

		case hasTag(line, tagDiscontinuity):
			pendingDiscont = true

		case hasTag(line, tagKey):
			curKey = parseKey(tagValue(line, tagKey), base)

		case hasTag(line, tagMap):
			attrs := parseAttributes(tagValue(line, tagMap))
			mi := &MapInfo{}
			if u, ok := attrs["URI"]; ok {
				mi.URI = resolveURI(base, u)
			}
			if br, ok := attrs["BYTERANGE"]; ok {
				mi.ByteRange, _ = parseByteRange(br, 0)
			}
			curMap = mi

		case hasTag(line, tagByteRange):
			// Resolved once the URI line is seen, since the implicit offset
			// depends on which URI the range applies to.
			pendingRange = &ByteRange{}
			pendingRange.Length, pendingRange.Offset = parseByteRangeParts(tagValue(line, tagByteRange))

		case hasTag(line, tagProgramDateTime):
			if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(tagValue(line, tagProgramDateTime))); err == nil {
				pendingPDT = t
			}

		case hasTag(line, tagInf):
			pendingDur, pendingTitle = parseInf(tagValue(line, tagInf))
			haveInf = true

		case strings.HasPrefix(line, "#"):
			// Unknown tag or comment: ignore.

		default:
			// A URI line completes the pending segment. A URI without a
			// preceding EXTINF is not a media segment; skip it.
			if !haveInf {
				continue
			}
			uri := resolveURI(base, line)

			seg := Segment{
				URI:             uri,
				Duration:        pendingDur,
				Title:           pendingTitle,
				SeqNo:           m.MediaSequence + uint64(len(m.Segments)),
				Discontinuity:   pendingDiscont,
				Key:             curKey,
				Map:             curMap,
				ProgramDateTime: pendingPDT,
			}
			if pendingRange != nil {
				br := *pendingRange
				// An absent offset means "immediately after the previous
				// sub-range of the same resource".
				if br.Offset == 0 {
					br.Offset = lastRangeEndFor[uri]
				}
				lastRangeEndFor[uri] = br.Offset + br.Length
				seg.ByteRange = &br
			}
			m.Segments = append(m.Segments, seg)

			pendingDur, pendingTitle = 0, ""
			pendingRange, pendingDiscont, haveInf = nil, false, false
			pendingPDT = time.Time{}
		}
	}

	return m, nil
}

// splitPlaylist normalises line endings, drops blank lines, and verifies the
// #EXTM3U preamble.
func splitPlaylist(data []byte) ([]string, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	raw := strings.Split(text, "\n")

	out := make([]string, 0, len(raw))
	for _, l := range raw {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	if len(out) == 0 || !hasTag(out[0], tagM3U) {
		return nil, errors.New("not an HLS playlist: missing #EXTM3U")
	}
	return out[1:], nil
}

// parseKey builds a Key from an EXT-X-KEY attribute list. METHOD=NONE returns
// nil so callers can treat "non-nil Key" as "encrypted".
func parseKey(attrList string, base *url.URL) *Key {
	attrs := parseAttributes(attrList)
	method := attrs["METHOD"]
	if method == "" || method == "NONE" {
		return nil
	}
	k := &Key{
		Method:            method,
		KeyFormat:         attrs["KEYFORMAT"],
		KeyFormatVersions: attrs["KEYFORMATVERSIONS"],
	}
	if u, ok := attrs["URI"]; ok && u != "" {
		k.URI = resolveURI(base, u)
	}
	if ivs, ok := attrs["IV"]; ok {
		ivs = strings.TrimPrefix(strings.TrimPrefix(ivs, "0x"), "0X")
		if b, err := hex.DecodeString(ivs); err == nil {
			k.IV = b
		}
	}
	return k
}

// parseInf splits an EXTINF value into duration and title.
func parseInf(v string) (float64, string) {
	durStr, title := v, ""
	if i := strings.Index(v, ","); i >= 0 {
		durStr, title = v[:i], strings.TrimSpace(v[i+1:])
	}
	d, _ := strconv.ParseFloat(strings.TrimSpace(durStr), 64)
	return d, title
}

// parseByteRangeParts parses "<length>[@<offset>]". A missing offset yields 0,
// which the caller resolves against the previous range for the same URI.
func parseByteRangeParts(v string) (length, offset uint64) {
	v = strings.TrimSpace(v)
	lenStr, offStr := v, ""
	if i := strings.Index(v, "@"); i >= 0 {
		lenStr, offStr = v[:i], v[i+1:]
	}
	length, _ = strconv.ParseUint(strings.TrimSpace(lenStr), 10, 64)
	if offStr != "" {
		offset, _ = strconv.ParseUint(strings.TrimSpace(offStr), 10, 64)
	}
	return length, offset
}

// parseByteRange parses a BYTERANGE attribute value into a ByteRange.
func parseByteRange(v string, defaultOffset uint64) (*ByteRange, bool) {
	length, offset := parseByteRangeParts(v)
	if length == 0 {
		return nil, false
	}
	if offset == 0 {
		offset = defaultOffset
	}
	return &ByteRange{Length: length, Offset: offset}, true
}

// parseResolution parses a "<width>x<height>" attribute value.
func parseResolution(v string) (w, h int) {
	if v == "" {
		return 0, 0
	}
	i := strings.IndexAny(v, "xX")
	if i < 0 {
		return 0, 0
	}
	w, _ = strconv.Atoi(strings.TrimSpace(v[:i]))
	h, _ = strconv.Atoi(strings.TrimSpace(v[i+1:]))
	return w, h
}

// parseAttributes parses an HLS attribute list into a map, honouring quoted
// values so that commas inside them (CODECS, NAME) do not split attributes.
// Surrounding quotes are stripped from the returned values.
func parseAttributes(s string) map[string]string {
	attrs := make(map[string]string)
	for _, part := range splitUnquoted(s, ',') {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.Index(part, "=")
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(part[:i])
		val := strings.TrimSpace(part[i+1:])
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		attrs[key] = val
	}
	return attrs
}

// splitUnquoted splits s on sep, ignoring separators inside double quotes.
func splitUnquoted(s string, sep byte) []string {
	var out []string
	var start int
	var inQuotes bool
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuotes = !inQuotes
		case sep:
			if !inQuotes {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// resolveURI resolves a playlist URI reference against the playlist's own URL.
// Using net/url.ResolveReference (rather than string concatenation) is what
// makes "../", root-relative "/path", and query-bearing URIs come out right.
func resolveURI(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	u, err := url.Parse(ref)
	if err != nil {
		// Unparseable reference: fall back to the raw text so the failure
		// surfaces at fetch time with a useful URL in the log.
		return ref
	}
	if base == nil {
		return u.String()
	}
	return base.ResolveReference(u).String()
}
