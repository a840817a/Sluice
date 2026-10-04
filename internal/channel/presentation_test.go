package channel

import (
	"context"
	"errors"
	"testing"

	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/hlsingest"
	"github.com/a840817a/sluice/internal/hlskey"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/presentation"
)

// A DASH source whose MPD has not been fetched yet is servable both ways but
// not ready.
func TestTracksBeforeFirstMPD(t *testing.T) {
	rt := &Runtime{Index: index.NewChannelIndex(index.PublishAVBarrier)}
	if p := rt.Serving().Tracks; !p.ServesHLS || !p.ServesDASH || p.Ready {
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
	ci := index.NewChannelIndex(index.PublishAVBarrier)
	ci.Period("0").AS("0").Rep("v0", 8).RestorePublished(seg(1))
	sv := Serving{Channel: presentation.Channel{IsHLSSource: true, Tracks: presentation.Tracks{
		Variants: []hlsdesc.Variant{{RepID: "v0", Codecs: "avc1.64001f", Width: 1280, Height: 720}},
	}}}

	tracks := buildTracks(sv, ci)
	if len(tracks) != 1 || tracks[0].Codecs != "avc1.64001f" || tracks[0].Width != 1280 {
		t.Errorf("tracks = %+v, want v0 decorated from the presentation", tracks)
	}
}
