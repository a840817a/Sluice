package manifest

import (
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

// A SegmentList upstream is republished as a SegmentTemplate: the segments are
// already on disk under the gateway's numbered layout, so the output addresses
// them the same way every other source does. A player never learns the upstream
// used SegmentList.

const segmentListUpstream = `<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static"
     mediaPresentationDuration="PT12S" minBufferTime="PT2S"
     profiles="urn:mpeg:dash:profile:isoff-on-demand:2011">
  <Period id="p0">
    <AdaptationSet id="1" mimeType="video/mp4" contentType="video">
      <Representation id="v0" bandwidth="800000" codecs="avc1.64001f" width="640" height="360">
        <SegmentList timescale="90000" duration="360000" startNumber="1">
          <Initialization sourceURL="init.mp4"/>
          <SegmentURL media="seg-1.m4s"/>
          <SegmentURL media="seg-2.m4s"/>
          <SegmentURL media="seg-3.m4s"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

// publishedIndex returns an index holding segNos as published segments of the
// first AdaptationSet in period p0.
//
// The AdaptationSet key is "0", not the id="1" the MPD carries: this parser
// numbers AdaptationSets by position across the whole manifest rather than
// trusting the upstream's attribute, so that the index key is stable even for a
// manifest that reuses or omits ids.
func publishedIndex(t *testing.T, timescale uint32, durTicks int64, segNos ...uint64) index.Snapshot {
	t.Helper()
	ci := index.NewChannelIndex(index.PublishAVBarrier)
	rep := ci.Period("p0").AS("0").Rep("v0", 32)

	published := map[uint64]struct{}{}
	for _, n := range segNos {
		start := int64(n-1) * durTicks
		rep.RestorePublished(index.SegmentState{
			SegNo: n, StartPTS: start, EndPTS: start + durTicks,
			Timescale: timescale, Path: "/data/ch/v0/" + string(rune('0'+n)) + ".m4s",
		})
		published[n] = struct{}{}
	}
	return ci.Snapshot()
}

func generateOrFail(t *testing.T, raw string, snap index.Snapshot) string {
	t.Helper()
	p, err := imdp.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := Generate(p, snap, GeneratorConfig{
		GatewayBaseURL: "https://gw.example.com",
		ChannelID:      "ch1",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return string(out)
}

func TestSegmentListIsRepublishedAsAGatewaySegmentTemplate(t *testing.T) {
	snap := publishedIndex(t, 90000, 360000, 1, 2, 3)
	xml := generateOrFail(t, segmentListUpstream, snap)

	if strings.Contains(xml, "<SegmentList") {
		t.Error("output still carries a SegmentList; the gateway serves numbered segments")
	}
	if !strings.Contains(xml, "<SegmentTemplate") {
		t.Fatalf("output has no SegmentTemplate:\n%s", xml)
	}
	// The upstream's own segment filenames must not survive into the output.
	for _, upstream := range []string{"seg-1.m4s", "seg-2.m4s", "seg-3.m4s", "init.mp4"} {
		if strings.Contains(xml, upstream) {
			t.Errorf("output leaks the upstream filename %q", upstream)
		}
	}
	if !strings.Contains(xml, "v1/channels/ch1/segments/") {
		t.Errorf("output does not address the gateway's own segment paths:\n%s", xml)
	}
	if !strings.Contains(xml, `timescale="90000"`) {
		t.Errorf("timescale from the SegmentList was not carried over:\n%s", xml)
	}
	if !strings.Contains(xml, `duration="360000"`) {
		t.Errorf("fixed duration from the SegmentList was not carried over:\n%s", xml)
	}
}

// startNumber follows what is actually published, not the upstream's numbering:
// after a restart or a window slide the first segment the gateway holds may not
// be segment 1, and announcing 1 would point players at a segment that is gone.
func TestSegmentListStartNumberFollowsWhatIsPublished(t *testing.T) {
	snap := publishedIndex(t, 90000, 360000, 5, 6, 7)
	xml := generateOrFail(t, segmentListUpstream, snap)

	if !strings.Contains(xml, `startNumber="5"`) {
		t.Errorf("startNumber does not match the first published segment:\n%s", xml)
	}
}

// Unlike a SegmentTemplate upstream, there is no template to fall back on, so a
// representation with nothing published yet is dropped rather than advertised
// as something a player can request.
func TestSegmentListRepresentationIsDroppedUntilSomethingIsPublished(t *testing.T) {
	xml := generateOrFail(t, segmentListUpstream, index.NewChannelIndex(index.PublishAVBarrier).Snapshot())

	if strings.Contains(xml, `id="v0"`) {
		t.Errorf("a representation with nothing published was advertised:\n%s", xml)
	}
}

// A SegmentList that states no per-segment duration gets a SegmentTimeline
// built from what was actually stored, because there is no fixed duration to
// announce.
func TestSegmentListWithoutDurationGetsATimeline(t *testing.T) {
	const noDuration = `<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static"
     mediaPresentationDuration="PT12S" minBufferTime="PT2S"
     profiles="urn:mpeg:dash:profile:isoff-on-demand:2011">
  <Period id="p0">
    <AdaptationSet id="1" mimeType="video/mp4" contentType="video">
      <Representation id="v0" bandwidth="800000" codecs="avc1.64001f" width="640" height="360">
        <SegmentList timescale="90000">
          <Initialization sourceURL="init.mp4"/>
          <SegmentURL media="seg-1.m4s"/>
          <SegmentURL media="seg-2.m4s"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

	snap := publishedIndex(t, 90000, 360000, 1, 2)
	xml := generateOrFail(t, noDuration, snap)

	if !strings.Contains(xml, "<SegmentTimeline") {
		t.Errorf("no timeline for a SegmentList without a stated duration:\n%s", xml)
	}
	if strings.Contains(xml, `duration="`) && strings.Contains(xml, "<SegmentTemplate") {
		// A fixed duration and a timeline together are contradictory.
		if idx := strings.Index(xml, "<SegmentTemplate"); idx >= 0 {
			tmpl := xml[idx:]
			if end := strings.Index(tmpl, ">"); end >= 0 && strings.Contains(tmpl[:end], `duration="`) {
				t.Errorf("SegmentTemplate announces both a fixed duration and a timeline:\n%s", xml)
			}
		}
	}
}

// The timescale is what every timing number in the output is expressed in. When
// the SegmentList omits it, the value recorded with the stored segments is the
// only honest source.
func TestSegmentListTimescaleFallsBackToTheIndex(t *testing.T) {
	const noTimescale = `<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static"
     mediaPresentationDuration="PT12S" minBufferTime="PT2S"
     profiles="urn:mpeg:dash:profile:isoff-on-demand:2011">
  <Period id="p0">
    <AdaptationSet id="1" mimeType="video/mp4" contentType="video">
      <Representation id="v0" bandwidth="800000" codecs="avc1.64001f" width="640" height="360">
        <SegmentList>
          <SegmentURL media="seg-1.m4s"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

	snap := publishedIndex(t, 48000, 96000, 1)
	xml := generateOrFail(t, noTimescale, snap)

	if !strings.Contains(xml, `timescale="48000"`) {
		t.Errorf("timescale did not fall back to the one recorded with the segments:\n%s", xml)
	}
}
