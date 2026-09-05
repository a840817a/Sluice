package manifest

import (
	"os"
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

// An empty GatewayBaseURL is the documented default. DASH resolves the MPD's
// <BaseURL> against the URL the manifest was fetched from (RFC 3986), so "/"
// makes one manifest correct at localhost, at a LAN IP over TLS, and behind a
// reverse proxy, without the gateway knowing any of those addresses.
func TestGenerateEmptyBaseIsRootRelative(t *testing.T) {
	data, err := os.ReadFile("../../testdata/manifest.mpd")
	if err != nil {
		t.Fatal(err)
	}
	p, err := imdp.Parse(data)
	if err != nil {
		t.Fatal(err)
	}

	ci := index.NewChannelIndex()
	out, err := Generate(p, ci.Snapshot(), GeneratorConfig{ChannelID: "ch1"})
	if err != nil {
		t.Fatal(err)
	}
	xml := string(out)

	if !strings.Contains(xml, "<BaseURL>/</BaseURL>") {
		t.Errorf("expected a root BaseURL element, got:\n%s", xml)
	}
	// Segment and init templates are relative to <BaseURL> and must stay that
	// way; an absolute gateway URL anywhere means the base leaked into them.
	for _, bad := range []string{"http://", "https://gw."} {
		if strings.Contains(xml, bad) {
			t.Errorf("empty base still produced an absolute gateway URI (%q):\n%s", bad, xml)
		}
	}
	if !strings.Contains(xml, "v1/channels/ch1/segments/") {
		t.Errorf("segment template missing:\n%s", xml)
	}
}

// A dynamic, SegmentTimeline-driven source has no fixed segment duration, so
// the timeline is the ONLY segment info its representations ever carry. Gating
// that on GatewayBaseURL made the documented default (empty base, relative
// URLs) emit a SegmentTemplate with neither duration nor timeline — which
// Shaka rejects with DASH_NO_SEGMENT_INFO (4002) before it ever asks for a
// license key.
func TestTimelineSurvivesRelativeBase(t *testing.T) {
	const src = `<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic" minimumUpdatePeriod="PT2S"
     availabilityStartTime="2024-01-01T00:00:00" minBufferTime="PT2S"
     profiles="urn:mpeg:dash:profile:isoff-live:2011">
  <Period start="PT0S" id="p0">
    <AdaptationSet mimeType="video/mp4">
      <Representation id="v0" width="1920" height="1080" bandwidth="6000000" codecs="avc1.4d4028">
        <SegmentTemplate timescale="90000" media="up_$Number%09d$.m4s" initialization="up_init.mp4">
          <SegmentTimeline><S t="0" d="180000" r="2"/></SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

	p, err := imdp.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}

	ci := index.NewChannelIndex()
	as := ci.Period("p0").AS("0")
	as.SetMediaType(index.MediaVideo)
	rep := as.Rep("v0", 0)
	published := make(map[uint64]struct{})
	for n := uint64(1); n <= 3; n++ {
		rep.Commit(index.SegmentState{
			SegNo:     n,
			Timescale: 90000,
			StartPTS:  int64(n-1) * 180000,
			EndPTS:    int64(n) * 180000,
		})
		published[n] = struct{}{}
	}
	rep.MarkPublished(published)
	snap := ci.Snapshot()

	for _, base := range []string{"", "https://gw.example.com"} {
		out, err := Generate(p, snap, GeneratorConfig{GatewayBaseURL: base, ChannelID: "ch1"})
		if err != nil {
			t.Fatalf("base %q: %v", base, err)
		}
		xml := string(out)
		if !strings.Contains(xml, "<SegmentTimeline>") {
			t.Errorf("base %q: no SegmentTimeline emitted, so the representation "+
				"carries no segment info at all:\n%s", base, xml)
		}
	}
}
