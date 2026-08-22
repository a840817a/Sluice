package manifest

import (
	"strings"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

const cmafTimescale = 48000

// cmafIndex builds an index holding n published fMP4 segments per representation.
func cmafIndex(periodID string, repIDs []string, n int) *index.ChannelIndex {
	ci := index.NewChannelIndex()
	ps := ci.Period(periodID)
	as := ps.AS("0")
	as.SetMediaType("video")
	for _, repID := range repIDs {
		rep := as.Rep(repID, 0)
		for i := 0; i < n; i++ {
			rep.Commit(index.SegmentState{
				SegNo:     uint64(i),
				StartPTS:  int64(i) * 4 * cmafTimescale,
				EndPTS:    int64(i+1) * 4 * cmafTimescale,
				Timescale: cmafTimescale,
				Path:      "/data/ch1/periods/" + periodID + "/video_0/" + repID + "/x.m4s",
				InitPath:  "/data/ch1/periods/" + periodID + "/video_0/" + repID + "/init.mp4",
			})
		}
		segNos := make(map[uint64]struct{}, n)
		for i := 0; i < n; i++ {
			segNos[uint64(i)] = struct{}{}
		}
		rep.MarkPublished(segNos)
	}
	return ci
}

func synthOpts() HLSSynthOptions {
	return HLSSynthOptions{
		ASID:                  "0",
		MediaType:             imdp.MediaVideo,
		MimeType:              "video/mp4",
		AvailabilityStartTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		MinimumUpdatePeriod:   4 * time.Second,
		TimeShiftBufferDepth:  60 * time.Second,
	}
}

func testVariants() []HLSVariant {
	return []HLSVariant{
		{RepID: "v0", Bandwidth: 1280000, Codecs: "avc1.4d401f,mp4a.40.2", Width: 1280, Height: 720},
		{RepID: "v1", Bandwidth: 640000, Codecs: "avc1.4d401e,mp4a.40.2", Width: 640, Height: 360},
	}
}

func TestSynthesizeHLSMPDShape(t *testing.T) {
	ci := cmafIndex("0", []string{"v0", "v1"}, 3)
	p := SynthesizeHLSMPD(testVariants(), ci.Snapshot(), synthOpts())
	if p == nil {
		t.Fatal("SynthesizeHLSMPD returned nil")
	}

	if p.Type != imdp.PresentationDynamic {
		t.Errorf("Type = %v, want dynamic", p.Type)
	}
	if p.AvailabilityStartTime.IsZero() {
		t.Error("dynamic presentation has no availabilityStartTime")
	}
	if len(p.Periods) != 1 {
		t.Fatalf("len(Periods) = %d, want 1", len(p.Periods))
	}
	as := p.Periods[0].AdaptationSets[0]
	if as.ID != "0" || as.MediaType != imdp.MediaVideo || as.MimeType != "video/mp4" {
		t.Errorf("AdaptationSet = %+v", as)
	}
	if len(as.Representations) != 2 {
		t.Fatalf("len(Representations) = %d, want 2", len(as.Representations))
	}

	r := as.Representations[0]
	if r.ID != "v0" || r.Bandwidth != 1280000 || r.Width != 1280 || r.Height != 720 {
		t.Errorf("representation = %+v", r)
	}
	if r.Codecs != "avc1.4d401f,mp4a.40.2" {
		t.Errorf("Codecs = %q", r.Codecs)
	}
	// The timescale has to come from the index, since the generator builds its
	// timeline from indexed PTS values expressed in it.
	if r.SegTemplate == nil || r.SegTemplate.Timescale != cmafTimescale {
		t.Errorf("SegTemplate = %+v, want timescale %d", r.SegTemplate, cmafTimescale)
	}
}

func TestSynthesizeHLSMPDVOD(t *testing.T) {
	ci := cmafIndex("0", []string{"v0"}, 3)
	o := synthOpts()
	o.VOD = true
	p := SynthesizeHLSMPD(testVariants()[:1], ci.Snapshot(), o)
	if p == nil {
		t.Fatal("nil MPD")
	}
	if p.Type != imdp.PresentationStatic {
		t.Errorf("Type = %v, want static", p.Type)
	}
	if !p.AvailabilityStartTime.IsZero() {
		t.Error("static presentation must not carry an availabilityStartTime")
	}
}

func TestSynthesizeHLSMPDSkipsRepsWithNoSegments(t *testing.T) {
	// A variant discovered in the master playlist but not yet downloaded has no
	// timescale and nothing to put on a timeline; advertising it would send
	// players after segments that do not exist.
	ci := cmafIndex("0", []string{"v0"}, 2)
	p := SynthesizeHLSMPD(testVariants(), ci.Snapshot(), synthOpts())
	if p == nil {
		t.Fatal("nil MPD")
	}
	reps := p.Periods[0].AdaptationSets[0].Representations
	if len(reps) != 1 || reps[0].ID != "v0" {
		t.Fatalf("representations = %+v, want only v0", reps)
	}
}

func TestSynthesizeHLSMPDNilWhenNothingPublished(t *testing.T) {
	if p := SynthesizeHLSMPD(testVariants(), index.NewChannelIndex().Snapshot(), synthOpts()); p != nil {
		t.Errorf("expected nil for an empty index, got %+v", p)
	}
	if p := SynthesizeHLSMPD(nil, cmafIndex("0", []string{"v0"}, 2).Snapshot(), synthOpts()); p != nil {
		t.Error("expected nil when there are no variants")
	}
}

func TestSynthesizeHLSMPDPeriodsAreStablyOrdered(t *testing.T) {
	// Upstream resets create extra periods. Map iteration order must not leak
	// into the presentation, or the period order would change per request.
	ci := cmafIndex("0", []string{"v0"}, 2)
	for _, pid := range []string{"2", "1"} {
		ps := ci.Period(pid)
		as := ps.AS("0")
		as.SetMediaType("video")
		rep := as.Rep("v0", 0)
		rep.Commit(index.SegmentState{SegNo: 0, EndPTS: 4 * cmafTimescale, Timescale: cmafTimescale})
		rep.MarkPublished(map[uint64]struct{}{0: {}})
	}

	for i := 0; i < 5; i++ {
		p := SynthesizeHLSMPD(testVariants()[:1], ci.Snapshot(), synthOpts())
		if p == nil {
			t.Fatal("nil MPD")
		}
		var got []string
		for _, per := range p.Periods {
			got = append(got, per.ID)
		}
		if strings.Join(got, ",") != "0,1,2" {
			t.Fatalf("period order = %v, want 0,1,2 on every call", got)
		}
	}
}

// TestSynthesizedMPDGeneratesPlayableManifest is the point of the whole
// exercise: the stand-in must drive the real generator to a manifest whose
// segment URLs and timeline are correct.
func TestSynthesizedMPDGeneratesPlayableManifest(t *testing.T) {
	ci := cmafIndex("0", []string{"v0", "v1"}, 3)
	p := SynthesizeHLSMPD(testVariants(), ci.Snapshot(), synthOpts())

	out, err := Generate(p, ci.Snapshot(), GeneratorConfig{
		GatewayBaseURL:              "https://gw.example.com",
		ChannelID:                   "ch1",
		WindowDepth:                 time.Hour,
		FallbackMinimumUpdatePeriod: 4 * time.Second,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	xml := string(out)

	for _, want := range []string{
		`type="dynamic"`,
		`mimeType="video/mp4"`,
		`bandwidth="1280000"`,
		`width="1280"`,
		`codecs="avc1.4d401f,mp4a.40.2"`,
		`v1/channels/ch1/segments/v0/$Number%09d$.m4s`,
		`v1/channels/ch1/init/v0.mp4`,
		`<SegmentTimeline>`,
		`timescale="48000"`,
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("generated MPD missing %q:\n%s", want, xml)
		}
	}
	// Both representations must be present.
	if !strings.Contains(xml, `segments/v1/`) {
		t.Errorf("second representation missing:\n%s", xml)
	}
	// A 4 s segment at timescale 48000 is 192000 ticks.
	if !strings.Contains(xml, `d="192000"`) {
		t.Errorf("timeline duration not derived from the index:\n%s", xml)
	}
}

// commitAudioAS puts n published audio segments into AS "1" of an existing index.
func commitAudioAS(ci *index.ChannelIndex, periodID, repID string, n int) {
	ps := ci.Period(periodID)
	as := ps.AS("1")
	as.SetMediaType("audio")
	rep := as.Rep(repID, 0)
	for i := 0; i < n; i++ {
		rep.Commit(index.SegmentState{
			SegNo:     uint64(i),
			StartPTS:  int64(i) * 4 * cmafTimescale,
			EndPTS:    int64(i+1) * 4 * cmafTimescale,
			Timescale: cmafTimescale,
			Path:      "a.m4s",
			InitPath:  "ainit.mp4",
		})
		rep.MarkPublished(map[uint64]struct{}{uint64(i): {}})
	}
}

func TestSynthesizeHLSMPDDemuxedAudio(t *testing.T) {
	ci := cmafIndex("0", []string{"v0"}, 2)
	commitAudioAS(ci, "0", "a0", 2)

	p := SynthesizeHLSMPD(
		[]HLSVariant{{RepID: "v0", Bandwidth: 800000, Codecs: "avc1.42c01e,mp4a.40.2", Width: 640, Height: 360}},
		ci.Snapshot(),
		HLSSynthOptions{
			ASID: "0", MediaType: imdp.MediaVideo, MimeType: "video/mp4",
			Audio: []HLSAudio{{RepID: "a0"}}, AudioASID: "1", VOD: true,
		},
	)
	if p == nil || len(p.Periods) != 1 {
		t.Fatalf("periods = %v", p)
	}
	sets := p.Periods[0].AdaptationSets
	if len(sets) != 2 {
		t.Fatalf("AdaptationSets = %d, want 2 (video+audio)", len(sets))
	}
	video, audio := sets[0], sets[1]
	if video.MediaType != imdp.MediaVideo || audio.MediaType != imdp.MediaAudio {
		t.Fatalf("AS types = %v,%v", video.MediaType, audio.MediaType)
	}
	if audio.MimeType != "audio/mp4" {
		t.Errorf("audio mime = %q", audio.MimeType)
	}
	// Codecs must be split: video keeps avc1, audio keeps mp4a.
	if got := video.Representations[0].Codecs; got != "avc1.42c01e" {
		t.Errorf("video codecs = %q, want just avc1", got)
	}
	if got := audio.Representations[0].Codecs; got != "mp4a.40.2" {
		t.Errorf("audio codecs = %q, want just mp4a", got)
	}
}

func TestSynthesizeHLSMPDMuxedKeepsFullCodecs(t *testing.T) {
	// No audio renditions: the (muxed) variant keeps its full CODECS.
	ci := cmafIndex("0", []string{"v0"}, 1)
	p := SynthesizeHLSMPD(
		[]HLSVariant{{RepID: "v0", Bandwidth: 1, Codecs: "avc1.42c01e,mp4a.40.2"}},
		ci.Snapshot(), HLSSynthOptions{ASID: "0", MediaType: imdp.MediaVideo, MimeType: "video/mp4", VOD: true},
	)
	if n := len(p.Periods[0].AdaptationSets); n != 1 {
		t.Fatalf("muxed produced %d AS, want 1", n)
	}
	if got := p.Periods[0].AdaptationSets[0].Representations[0].Codecs; got != "avc1.42c01e,mp4a.40.2" {
		t.Errorf("muxed codecs = %q, want the full list", got)
	}
}
