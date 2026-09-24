package channel

import (
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/hlsingest"
	"github.com/a840817a/sluice/internal/index"
)

// Which index a channel serves from is a rule that used to live at every call
// site, and getting it wrong is invisible: a transitioned channel keeps
// answering, just from the live window instead of the full asset. These cases
// pin the rule where it now lives.
func TestServingResolvesIndex(t *testing.T) {
	live := index.NewChannelIndex()
	vod := index.NewChannelIndex()

	tests := []struct {
		name     string
		mode     ChannelMode
		vodIndex *index.ChannelIndex
		want     *index.ChannelIndex
	}{
		{"live serves the live index", ModeLive, nil, live},
		{"vod serves the full-history index", ModeVOD, vod, vod},
		{
			// The transition stores the index and flips the mode separately, so
			// a reader can arrive in between. Serving the live index is the only
			// safe answer; nil would be a panic in every caller.
			name: "vod without a built index falls back to live",
			mode: ModeVOD, vodIndex: nil, want: live,
		},
		{
			// Segments are still arriving and the VOD index does not describe
			// them yet. This exception is easy to "simplify" away because
			// nothing else observes it.
			name: "static ingest stays on the live index",
			mode: ModeStaticIngesting, vodIndex: vod, want: live,
		},
		{"transitioning stays on the live index", ModeTransitioning, vod, live},
		{"failed VOD keeps the recorded index", ModeVODFailed, nil, live},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := &Runtime{ID: "c", Index: live}
			driveMode(t, rt, tc.mode)
			if tc.vodIndex != nil {
				rt.vodIndex.Store(tc.vodIndex)
			}

			got := rt.Serving().Index
			if got != tc.want {
				t.Errorf("Serving().Index = %p, want %p (live=%p vod=%p)",
					got, tc.want, live, vod)
			}
		})
	}
}

func TestServingModePredicates(t *testing.T) {
	tests := []struct {
		mode                ChannelMode
		isVOD, isStaticIngs bool
	}{
		{ModeLive, false, false},
		{ModeTransitioning, false, false},
		{ModeVOD, true, false},
		{ModeStaticIngesting, false, true},
		{ModeVODFailed, false, false},
	}

	for _, tc := range tests {
		rt := &Runtime{Index: index.NewChannelIndex()}
		driveMode(t, rt, tc.mode)
		sv := rt.Serving()

		if sv.IsVOD() != tc.isVOD {
			t.Errorf("mode %v: IsVOD() = %v, want %v", tc.mode, sv.IsVOD(), tc.isVOD)
		}
		if sv.IsStaticIngesting() != tc.isStaticIngs {
			t.Errorf("mode %v: IsStaticIngesting() = %v, want %v",
				tc.mode, sv.IsStaticIngesting(), tc.isStaticIngs)
		}
	}
}

// Output capability follows the runtime that was actually built, not the
// configuration that asked for it.
func TestServingCapabilities(t *testing.T) {
	t.Run("dash source serves dash and no keys", func(t *testing.T) {
		rt := &Runtime{Index: index.NewChannelIndex()}
		sv := rt.Serving()

		if sv.IsHLSSource {
			t.Error("IsHLSSource = true for a channel with no HLS watcher")
		}
		if !sv.ServesDASH {
			t.Error("ServesDASH = false for a DASH channel")
		}
		if sv.CanServeKeys {
			t.Error("CanServeKeys = true without a key cache")
		}
	})

	t.Run("mpeg-ts hls source cannot serve dash", func(t *testing.T) {
		// A zero Watcher has not discovered fMP4 segments, which is exactly the
		// MPEG-TS case: serving it as DASH would require remuxing.
		rt := &Runtime{Index: index.NewChannelIndex(), HLS: &hlsingest.Watcher{}}
		sv := rt.Serving()

		if !sv.IsHLSSource {
			t.Error("IsHLSSource = false with an HLS watcher attached")
		}
		if sv.ServesDASH {
			t.Error("ServesDASH = true for an MPEG-TS HLS channel")
		}
		if sv.CanServeKeys {
			t.Error("CanServeKeys = true without a key cache")
		}
	})
}

// Serving is a value: reading it must not leave the caller holding something
// that changes when the channel does.
func TestServingIsASnapshot(t *testing.T) {
	rt := &Runtime{ID: "c", Index: index.NewChannelIndex()}
	rt.setConfig(Config{Title: "before"})
	rt.setStaticIngestStart(time.Unix(100, 0))

	sv := rt.Serving()

	driveMode(t, rt, ModeVOD)
	rt.setConfig(Config{Title: "after"})

	if sv.Mode != ModeLive {
		t.Errorf("Mode = %v after the channel moved on, want the value read at snapshot time", sv.Mode)
	}
	if sv.Config.Title != "before" {
		t.Errorf("Config.Title = %q, want %q", sv.Config.Title, "before")
	}
	if !sv.StaticIngestStart.Equal(time.Unix(100, 0)) {
		t.Errorf("StaticIngestStart = %v, want %v", sv.StaticIngestStart, time.Unix(100, 0))
	}
}
