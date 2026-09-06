package mpd

import (
	"testing"
)

// go-mpd drops SegmentList and SegmentBase entirely, so this gateway re-parses
// them from the raw XML and stitches them back on by position. These tests go
// through Parse rather than the converters directly: the stitching is the part
// most likely to break, and a converter test would not see it.

const segmentListMPD = `<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static"
     mediaPresentationDuration="PT12S" minBufferTime="PT2S"
     profiles="urn:mpeg:dash:profile:isoff-on-demand:2011">
  <Period id="p0">
    <AdaptationSet id="1" mimeType="video/mp4" contentType="video">
      <BaseURL>as-level/</BaseURL>
      <SegmentList timescale="90000" duration="360000" startNumber="7">
        <Initialization sourceURL="as-init.mp4"/>
        <SegmentURL media="as-1.m4s"/>
      </SegmentList>
      <Representation id="v0" bandwidth="800000" codecs="avc1.64001f" width="640" height="360">
        <BaseURL>rep-level/</BaseURL>
        <SegmentList timescale="90000" duration="360000">
          <Initialization sourceURL="init.mp4"/>
          <SegmentURL media="seg-1.m4s"/>
          <SegmentURL media="seg-2.m4s"/>
          <SegmentURL media="seg-3.m4s"/>
        </SegmentList>
      </Representation>
      <Representation id="v1" bandwidth="400000" codecs="avc1.64001f" width="320" height="180">
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

func parseOrFail(t *testing.T, raw string) *ParsedMPD {
	t.Helper()
	p, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func repByID(t *testing.T, as *ParsedAdaptationSet, id string) *ParsedRepresentation {
	t.Helper()
	for i := range as.Representations {
		if as.Representations[i].ID == id {
			return as.Representations[i]
		}
	}
	t.Fatalf("representation %q not found", id)
	return nil
}

func TestParseSegmentListSurvivesGoMPD(t *testing.T) {
	p := parseOrFail(t, segmentListMPD)

	if len(p.Periods) != 1 || len(p.Periods[0].AdaptationSets) != 1 {
		t.Fatalf("got %d periods", len(p.Periods))
	}
	as := p.Periods[0].AdaptationSets[0]
	rep := repByID(t, as, "v0")

	sl := rep.EffectiveSegmentList(as)
	if sl == nil {
		t.Fatal("rep-level SegmentList was dropped")
	}
	if sl.Timescale != 90000 || sl.Duration != 360000 {
		t.Errorf("timescale/duration = %d/%d, want 90000/360000", sl.Timescale, sl.Duration)
	}
	if sl.InitURL != "init.mp4" {
		t.Errorf("InitURL = %q, want %q", sl.InitURL, "init.mp4")
	}
	if len(sl.Segments) != 3 {
		t.Fatalf("got %d SegmentURL entries, want 3", len(sl.Segments))
	}
	for i, want := range []string{"seg-1.m4s", "seg-2.m4s", "seg-3.m4s"} {
		if sl.Segments[i].URL != want {
			t.Errorf("segment %d URL = %q, want %q", i, sl.Segments[i].URL, want)
		}
	}
}

// An omitted startNumber means 1. Defaulting it to 0 instead would shift every
// segment number in the republished manifest by one.
func TestParseSegmentListStartNumberDefaultsToOne(t *testing.T) {
	p := parseOrFail(t, segmentListMPD)
	as := p.Periods[0].AdaptationSets[0]

	if got := repByID(t, as, "v0").EffectiveSegmentList(as).StartNumber; got != 1 {
		t.Errorf("rep-level StartNumber = %d, want 1 (it is absent from the XML)", got)
	}
	// The AS-level list states 7 explicitly, so the default must not overwrite it.
	if got := as.SegList.StartNumber; got != 7 {
		t.Errorf("AS-level StartNumber = %d, want the declared 7", got)
	}
}

// A representation without its own SegmentList inherits the AdaptationSet's, and
// one with its own overrides it. Getting this backwards makes every rep fetch
// the same segments.
func TestEffectiveSegmentListPrefersTheRepresentation(t *testing.T) {
	p := parseOrFail(t, segmentListMPD)
	as := p.Periods[0].AdaptationSets[0]

	own := repByID(t, as, "v0").EffectiveSegmentList(as)
	if own.InitURL != "init.mp4" {
		t.Errorf("rep with its own SegmentList got InitURL %q, want the rep-level %q",
			own.InitURL, "init.mp4")
	}

	inherited := repByID(t, as, "v1").EffectiveSegmentList(as)
	if inherited == nil {
		t.Fatal("representation without its own SegmentList inherited nothing")
	}
	if inherited.InitURL != "as-init.mp4" {
		t.Errorf("inherited InitURL = %q, want the AS-level %q", inherited.InitURL, "as-init.mp4")
	}
}

// Same precedence for BaseURL, which decides what every relative segment URL
// resolves against.
func TestEffectiveBaseURLPrefersTheRepresentation(t *testing.T) {
	p := parseOrFail(t, segmentListMPD)
	as := p.Periods[0].AdaptationSets[0]

	if got := repByID(t, as, "v0").EffectiveBaseURL(as); got != "rep-level/" {
		t.Errorf("rep with its own BaseURL resolved to %q, want %q", got, "rep-level/")
	}
	if got := repByID(t, as, "v1").EffectiveBaseURL(as); got != "as-level/" {
		t.Errorf("rep without a BaseURL resolved to %q, want the AS-level %q", got, "as-level/")
	}
}

const segmentBaseMPD = `<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static"
     mediaPresentationDuration="PT30S" minBufferTime="PT2S"
     profiles="urn:mpeg:dash:profile:isoff-on-demand:2011">
  <Period id="p0">
    <AdaptationSet id="1" mimeType="video/mp4" contentType="video">
      <Representation id="v0" bandwidth="800000" codecs="avc1.64001f" width="640" height="360">
        <BaseURL>video-single-file.mp4</BaseURL>
        <SegmentBase timescale="90000" indexRange="836-1200">
          <Initialization range="0-835"/>
        </SegmentBase>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

// A SegmentBase representation is one file: the init is a byte range at its
// head and the sidx names every segment inside it. Losing either range means
// the gateway cannot address the media at all.
func TestParseSegmentBaseSurvivesGoMPD(t *testing.T) {
	p := parseOrFail(t, segmentBaseMPD)
	as := p.Periods[0].AdaptationSets[0]
	rep := repByID(t, as, "v0")

	sb := rep.EffectiveSegmentBase(as)
	if sb == nil {
		t.Fatal("SegmentBase was dropped")
	}
	if sb.Timescale != 90000 {
		t.Errorf("Timescale = %d, want 90000", sb.Timescale)
	}
	if sb.IndexRange != "836-1200" {
		t.Errorf("IndexRange = %q, want %q", sb.IndexRange, "836-1200")
	}
	if sb.InitRange != "0-835" {
		t.Errorf("InitRange = %q, want %q", sb.InitRange, "0-835")
	}
	if got := rep.EffectiveBaseURL(as); got != "video-single-file.mp4" {
		t.Errorf("BaseURL = %q, want the single file the ranges address", got)
	}
}

// Byte-range SegmentList: the SegmentURL entries carry mediaRange instead of
// distinct URLs, so every segment lives in the file BaseURL names.
func TestParseSegmentListWithByteRanges(t *testing.T) {
	const raw = `<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static"
     mediaPresentationDuration="PT8S" minBufferTime="PT2S"
     profiles="urn:mpeg:dash:profile:isoff-on-demand:2011">
  <Period id="p0">
    <AdaptationSet id="1" mimeType="video/mp4" contentType="video">
      <Representation id="v0" bandwidth="800000" codecs="avc1.64001f" width="640" height="360">
        <BaseURL>all-in-one.mp4</BaseURL>
        <SegmentList timescale="90000" duration="360000">
          <Initialization range="0-835"/>
          <SegmentURL mediaRange="836-5000"/>
          <SegmentURL mediaRange="5001-9200"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

	p := parseOrFail(t, raw)
	as := p.Periods[0].AdaptationSets[0]
	sl := repByID(t, as, "v0").EffectiveSegmentList(as)

	if sl == nil {
		t.Fatal("SegmentList was dropped")
	}
	if sl.InitRange != "0-835" {
		t.Errorf("InitRange = %q, want %q", sl.InitRange, "0-835")
	}
	if sl.InitURL != "" {
		t.Errorf("InitURL = %q, want empty: this init is a range, not a separate file", sl.InitURL)
	}
	if len(sl.Segments) != 2 {
		t.Fatalf("got %d segments, want 2", len(sl.Segments))
	}
	for i, want := range []string{"836-5000", "5001-9200"} {
		if sl.Segments[i].MediaRange != want {
			t.Errorf("segment %d MediaRange = %q, want %q", i, sl.Segments[i].MediaRange, want)
		}
		if sl.Segments[i].URL != "" {
			t.Errorf("segment %d URL = %q, want empty: the range addresses BaseURL", i, sl.Segments[i].URL)
		}
	}
}

// The re-parsed SegmentList/SegmentBase elements are matched back onto go-mpd's
// tree by position, so a second period or a second AdaptationSet must land on
// the right one. Misalignment here would silently give a representation another
// one's segment list.
func TestSegmentListStitchingSurvivesMultiplePeriodsAndSets(t *testing.T) {
	const raw = `<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static"
     mediaPresentationDuration="PT20S" minBufferTime="PT2S"
     profiles="urn:mpeg:dash:profile:isoff-on-demand:2011">
  <Period id="p0">
    <AdaptationSet id="1" mimeType="video/mp4" contentType="video">
      <Representation id="v0" bandwidth="800000" codecs="avc1.64001f" width="640" height="360">
        <SegmentList timescale="90000" duration="360000">
          <SegmentURL media="p0-video-1.m4s"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
    <AdaptationSet id="2" mimeType="audio/mp4" contentType="audio">
      <Representation id="a0" bandwidth="128000" codecs="mp4a.40.2">
        <SegmentList timescale="48000" duration="192000">
          <SegmentURL media="p0-audio-1.m4s"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
  <Period id="p1">
    <AdaptationSet id="1" mimeType="video/mp4" contentType="video">
      <Representation id="v0" bandwidth="800000" codecs="avc1.64001f" width="640" height="360">
        <SegmentList timescale="90000" duration="360000">
          <SegmentURL media="p1-video-1.m4s"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

	p := parseOrFail(t, raw)
	if len(p.Periods) != 2 {
		t.Fatalf("got %d periods, want 2", len(p.Periods))
	}

	want := map[string]map[string]string{
		"p0": {"v0": "p0-video-1.m4s", "a0": "p0-audio-1.m4s"},
		"p1": {"v0": "p1-video-1.m4s"},
	}
	for _, period := range p.Periods {
		for i := range period.AdaptationSets {
			as := period.AdaptationSets[i]
			for j := range as.Representations {
				rep := as.Representations[j]
				sl := rep.EffectiveSegmentList(as)
				if sl == nil || len(sl.Segments) == 0 {
					t.Errorf("period %s rep %s lost its SegmentList", period.ID, rep.ID)
					continue
				}
				if got := sl.Segments[0].URL; got != want[period.ID][rep.ID] {
					t.Errorf("period %s rep %s first segment = %q, want %q — "+
						"the positional stitching put another element's list here",
						period.ID, rep.ID, got, want[period.ID][rep.ID])
				}
			}
		}
	}
}
