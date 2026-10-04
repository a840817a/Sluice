package presentation

import (
	"os"
	"testing"

	imdp "github.com/a840817a/sluice/internal/mpd"
)

func mpdRep(id string, bw uint64, w, h uint64, codecs string) *imdp.ParsedRepresentation {
	return &imdp.ParsedRepresentation{ID: id, Bandwidth: bw, Width: w, Height: h, Codecs: codecs}
}

func TestTracksFromMPDVideoAndAudio(t *testing.T) {
	p := &imdp.ParsedMPD{Periods: []*imdp.ParsedPeriod{{
		ID: "0",
		AdaptationSets: []*imdp.ParsedAdaptationSet{
			{MediaType: imdp.MediaVideo, MimeType: "video/mp4", Representations: []*imdp.ParsedRepresentation{
				mpdRep("v0", 6000000, 1920, 1080, "avc1.4d4028"),
				mpdRep("v1", 2000000, 1280, 720, "avc1.4d401f"),
			}},
			{MediaType: imdp.MediaAudio, MimeType: "audio/mp4", Lang: "en", Representations: []*imdp.ParsedRepresentation{
				mpdRep("a0", 128000, 0, 0, "mp4a.40.2"),
			}},
		},
	}}}

	v := TracksFromMPD(p)
	if !v.ServesHLS || !v.Ready {
		t.Fatal("clear DASH should be serveable as HLS")
	}
	if len(v.Variants) != 2 {
		t.Fatalf("variants = %d, want 2", len(v.Variants))
	}
	if v.Variants[0].RepID != "v0" || v.Variants[0].Width != 1920 || v.Variants[0].Codecs != "avc1.4d4028" {
		t.Errorf("variant 0 = %+v", v.Variants[0])
	}
	// Both variants must bind the synthesized audio group.
	for _, vr := range v.Variants {
		if vr.AudioGroup != "audio" {
			t.Errorf("variant %s AudioGroup = %q, want audio", vr.RepID, vr.AudioGroup)
		}
	}
	if len(v.Renditions) != 1 || v.Renditions[0].RepID != "a0" || v.Renditions[0].GroupID != "audio" {
		t.Fatalf("renditions = %+v", v.Renditions)
	}
	if v.Renditions[0].Language != "en" || !v.Renditions[0].Default {
		t.Errorf("audio rendition = %+v, want en/default", v.Renditions[0])
	}
}

func TestTracksFromMPDMuxedNoAudioGroup(t *testing.T) {
	// No separate audio AS: variants must not reference an audio group.
	p := &imdp.ParsedMPD{Periods: []*imdp.ParsedPeriod{{
		AdaptationSets: []*imdp.ParsedAdaptationSet{
			{MediaType: imdp.MediaVideo, Representations: []*imdp.ParsedRepresentation{
				mpdRep("v0", 3000000, 1280, 720, "avc1.4d401f,mp4a.40.2"),
			}},
		},
	}}}
	v := TracksFromMPD(p)
	if !v.ServesHLS {
		t.Fatal("clear DASH should be serveable")
	}
	if v.Variants[0].AudioGroup != "" {
		t.Errorf("muxed variant must not bind an audio group, got %q", v.Variants[0].AudioGroup)
	}
	if len(v.Renditions) != 0 {
		t.Errorf("muxed source produced %d renditions, want 0", len(v.Renditions))
	}
}

func TestTracksFromMPDRejectsEncrypted(t *testing.T) {
	// A ContentProtection anywhere means the presentation is encrypted, whose
	// HLS DRM signalling is out of scope: it must not be serveable as HLS.
	p := &imdp.ParsedMPD{Periods: []*imdp.ParsedPeriod{{
		AdaptationSets: []*imdp.ParsedAdaptationSet{{
			MediaType: imdp.MediaVideo,
			RawCPs:    []string{`<ContentProtection schemeIdUri="urn:uuid:..."/>`},
			Representations: []*imdp.ParsedRepresentation{
				mpdRep("v0", 3000000, 1280, 720, "avc1.4d401f"),
			},
		}},
	}}}
	if TracksFromMPD(p).ServesHLS {
		t.Error("encrypted DASH must not be serveable as HLS")
	}
}

func TestTracksFromMPDDedupesAcrossPeriods(t *testing.T) {
	as := func() []*imdp.ParsedAdaptationSet {
		return []*imdp.ParsedAdaptationSet{{
			MediaType:       imdp.MediaVideo,
			Representations: []*imdp.ParsedRepresentation{mpdRep("v0", 3000000, 1280, 720, "avc1")},
		}}
	}
	p := &imdp.ParsedMPD{Periods: []*imdp.ParsedPeriod{
		{ID: "0", AdaptationSets: as()},
		{ID: "1", AdaptationSets: as()},
	}}
	v := TracksFromMPD(p)
	if len(v.Variants) != 1 {
		t.Errorf("variants = %d, want 1 (deduped across periods)", len(v.Variants))
	}
}

func TestTracksFromMPDRejectsRealDRMManifest(t *testing.T) {
	// The repo's manifest.mpd is a real PlayReady/Widevine-protected MPD; the
	// parser must surface its ContentProtection so HLS output is refused.
	data, err := os.ReadFile("../../testdata/manifest.mpd")
	if err != nil {
		t.Skipf("manifest fixture unavailable: %v", err)
	}
	p, err := imdp.Parse(data)
	if err != nil {
		t.Fatalf("parse manifest.mpd: %v", err)
	}
	if TracksFromMPD(p).ServesHLS {
		t.Error("a DRM-protected DASH manifest must not be serveable as HLS")
	}
}

// An encrypted DASH source still serves DASH but lists no HLS tracks, so it
// is not ready.
func TestTracksFromMPDEncrypted(t *testing.T) {
	enc := &imdp.ParsedMPD{Periods: []*imdp.ParsedPeriod{{AdaptationSets: []*imdp.ParsedAdaptationSet{{
		MediaType:       imdp.MediaVideo,
		RawCPs:          []string{`<ContentProtection schemeIdUri="urn:uuid:..."/>`},
		Representations: []*imdp.ParsedRepresentation{mpdRep("v0", 1, 1, 1, "avc1")},
	}}}}}
	if p := TracksFromMPD(enc); p.ServesHLS || !p.ServesDASH || p.Ready {
		t.Errorf("encrypted DASH: ServesHLS %v ServesDASH %v Ready %v, want false true false", p.ServesHLS, p.ServesDASH, p.Ready)
	}
}
