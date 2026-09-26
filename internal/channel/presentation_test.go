package channel

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/hlsingest"
	"github.com/a840817a/sluice/internal/hlskey"
	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

func mpdRep(id string, bw uint64, w, h uint64, codecs string) *imdp.ParsedRepresentation {
	return &imdp.ParsedRepresentation{ID: id, Bandwidth: bw, Width: w, Height: h, Codecs: codecs}
}

func TestDASHPresentationVideoAndAudio(t *testing.T) {
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

	v := dashPresentation(p)
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

func TestDASHPresentationMuxedNoAudioGroup(t *testing.T) {
	// No separate audio AS: variants must not reference an audio group.
	p := &imdp.ParsedMPD{Periods: []*imdp.ParsedPeriod{{
		AdaptationSets: []*imdp.ParsedAdaptationSet{
			{MediaType: imdp.MediaVideo, Representations: []*imdp.ParsedRepresentation{
				mpdRep("v0", 3000000, 1280, 720, "avc1.4d401f,mp4a.40.2"),
			}},
		},
	}}}
	v := dashPresentation(p)
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

func TestDASHPresentationRejectsEncrypted(t *testing.T) {
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
	if dashPresentation(p).ServesHLS {
		t.Error("encrypted DASH must not be serveable as HLS")
	}
}

func TestDASHPresentationDedupesAcrossPeriods(t *testing.T) {
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
	v := dashPresentation(p)
	if len(v.Variants) != 1 {
		t.Errorf("variants = %d, want 1 (deduped across periods)", len(v.Variants))
	}
}

func TestDASHPresentationRejectsRealDRMManifest(t *testing.T) {
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
	if dashPresentation(p).ServesHLS {
		t.Error("a DRM-protected DASH manifest must not be serveable as HLS")
	}
}

// An encrypted DASH source still serves DASH but lists no HLS tracks, so it
// is not ready; a DASH source whose MPD has not been fetched yet is servable
// both ways but not ready.
func TestDASHPresentationProtocols(t *testing.T) {
	enc := &imdp.ParsedMPD{Periods: []*imdp.ParsedPeriod{{AdaptationSets: []*imdp.ParsedAdaptationSet{{
		MediaType:       imdp.MediaVideo,
		RawCPs:          []string{`<ContentProtection schemeIdUri="urn:uuid:..."/>`},
		Representations: []*imdp.ParsedRepresentation{mpdRep("v0", 1, 1, 1, "avc1")},
	}}}}}
	if p := dashPresentation(enc); p.ServesHLS || !p.ServesDASH || p.Ready {
		t.Errorf("encrypted DASH: ServesHLS %v ServesDASH %v Ready %v, want false true false", p.ServesHLS, p.ServesDASH, p.Ready)
	}
	rt := &Runtime{Index: index.NewChannelIndex()}
	if p := rt.Serving().Presentation; !p.ServesHLS || !p.ServesDASH || p.Ready {
		t.Errorf("DASH before its first MPD = %+v, want servable both ways and not ready", p)
	}
}

// The key proxy serves only a passthrough HLS channel's own keys.
func TestResolveKeyRefusesWhatItMustNot(t *testing.T) {
	keys := hlskey.NewCache(nil, fetch.NewAtomicHeaders(nil), "")
	for _, tc := range []struct {
		name string
		rt   *Runtime
		want error
	}{
		{"DASH channel", &Runtime{}, ErrNoKeyProxy},
		{"HLS without a key cache", &Runtime{hls: &hlsingest.Watcher{}}, ErrNoKeyProxy},
		{"decrypt on ingest", func() *Runtime {
			rt := &Runtime{hls: &hlsingest.Watcher{}, keys: keys}
			rt.setConfig(Config{HLSKeyMode: KeyModeDecrypt})
			return rt
		}(), ErrNoKeyProxy},
		{"key ID never issued", func() *Runtime {
			rt := &Runtime{hls: &hlsingest.Watcher{}, keys: keys}
			rt.setConfig(Config{HLSKeyMode: KeyModePassthrough})
			return rt
		}(), ErrUnknownKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.rt.ResolveKey(context.Background(), "k1"); !errors.Is(err, tc.want) {
				t.Errorf("ResolveKey error = %v, want %v", err, tc.want)
			}
		})
	}
}

// Status decorates an HLS source's tracks from the same Presentation the
// playlists are rendered from.
func TestBuildTracksDecoratesHLSFromThePresentation(t *testing.T) {
	ci := index.NewChannelIndex()
	ci.Period("0").AS("0").Rep("v0", 8).RestorePublished(seg(1))
	sv := Serving{IsHLSSource: true, Presentation: Presentation{
		Variants: []hlsdesc.Variant{{RepID: "v0", Codecs: "avc1.64001f", Width: 1280, Height: 720}},
	}}

	tracks := buildTracks(sv, ci)
	if len(tracks) != 1 || tracks[0].Codecs != "avc1.64001f" || tracks[0].Width != 1280 {
		t.Errorf("tracks = %+v, want v0 decorated from the presentation", tracks)
	}
}
