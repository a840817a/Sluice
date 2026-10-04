package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/index"
)

func TestExpireOldSegments_UsesLiveEdgeWindow(t *testing.T) {
	ci := index.NewChannelIndex(index.PublishAVBarrier)
	ps := ci.Period("p0")
	as := ps.AS("v0")
	as.SetMediaType("video")
	rep := as.Rep("r0", 16)

	// EndSec: 10, 20, 30 (timescale=10)
	rep.RestorePublished(index.SegmentState{SegNo: 1, StartPTS: 0, EndPTS: 100, Timescale: 10})
	rep.RestorePublished(index.SegmentState{SegNo: 2, StartPTS: 100, EndPTS: 200, Timescale: 10})
	rep.RestorePublished(index.SegmentState{SegNo: 3, StartPTS: 200, EndPTS: 300, Timescale: 10})

	p := NewIntake(config.Config{
		Window: config.WindowConfig{
			Depth:          12 * time.Second,
			SafeEdgeBuffer: 3 * time.Second,
		},
	}, "ch1", ci)

	// live edge = 30s, cutoff = 30 - (12+3) = 15s
	// => seg#1 expires (EndSec=10), seg#2/#3 stay published.
	p.expireOldSegments(ps)

	published, committed, expired := rep.StatusCounts()
	if committed != 0 {
		t.Fatalf("committed=%d, want 0", committed)
	}
	if published != 2 {
		t.Fatalf("published=%d, want 2", published)
	}
	if expired != 1 {
		t.Fatalf("expired=%d, want 1", expired)
	}
}

func TestExpireOldSegments_NoPublished_NoChange(t *testing.T) {
	ci := index.NewChannelIndex(index.PublishAVBarrier)
	ps := ci.Period("p0")
	as := ps.AS("v0")
	as.SetMediaType("video")
	rep := as.Rep("r0", 8)
	// An audio AdaptationSet that has not received a segment yet holds the A/V
	// barrier, so the video segment stays committed and unpublished.
	ps.AS("a0").SetMediaType(index.MediaAudio)

	ci.Commit(index.RepRef{PeriodID: "p0", ASID: "v0", RepID: "r0"}, 8, index.SegmentState{SegNo: 1, StartPTS: 0, EndPTS: 100, Timescale: 10})

	p := NewIntake(config.Config{
		Window: config.WindowConfig{
			Depth:          10 * time.Second,
			SafeEdgeBuffer: 2 * time.Second,
		},
	}, "ch1", ci)

	p.expireOldSegments(ps)

	published, committed, expired := rep.StatusCounts()
	if published != 0 || committed != 1 || expired != 0 {
		t.Fatalf("counts changed unexpectedly: published=%d committed=%d expired=%d", published, committed, expired)
	}
}

// expiryFixture builds a period with three published segments backed by real
// files, ending at 10s, 20s and 30s.
func expiryFixture(t *testing.T) (*index.ChannelIndex, *index.PeriodState, []string) {
	t.Helper()
	dir := t.TempDir()
	ci := index.NewChannelIndex(index.PublishAVBarrier)
	ps := ci.Period("p0")
	as := ps.AS("v0")
	as.SetMediaType("video")
	rep := as.Rep("r0", 16)

	var paths []string
	for i := 1; i <= 3; i++ {
		path := filepath.Join(dir, fmt.Sprintf("%d.m4s", i))
		if err := os.WriteFile(path, []byte("segment"), 0o644); err != nil {
			t.Fatalf("write fixture segment: %v", err)
		}
		paths = append(paths, path)
		rep.RestorePublished(index.SegmentState{
			SegNo: uint64(i), StartPTS: int64(i-1) * 100, EndPTS: int64(i) * 100,
			Timescale: 10, Path: path,
		})
	}
	return ci, ps, paths
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// The only code in this repo that deletes a viewer-visible file. live edge = 30s,
// cutoff = 30 - (12+3) = 15s, so segment 1 (ending at 10s) leaves the window.
func TestExpireOldSegments_OnExpireDeletesOnlyWhatLeftTheWindow(t *testing.T) {
	ci, ps, paths := expiryFixture(t)

	p := NewIntake(config.Config{
		Window: config.WindowConfig{Depth: 12 * time.Second, SafeEdgeBuffer: 3 * time.Second},
		Store:  config.StoreConfig{Cleanup: config.CleanupOnExpire},
	}, "ch1", ci)

	p.expireOldSegments(ps)

	if exists(t, paths[0]) {
		t.Errorf("segment 1 left the window but its file is still on disk")
	}
	for _, i := range []int{1, 2} {
		if !exists(t, paths[i]) {
			t.Errorf("segment %d is still inside the window but its file was deleted", i+1)
		}
	}
}

// The default mode expires in the index but must never touch the filesystem:
// an operator who has not asked for deletion keeps every byte.
func TestExpireOldSegments_DisabledNeverDeletes(t *testing.T) {
	for _, mode := range []config.CleanupMode{config.CleanupDisabled, "", "something-unrecognised"} {
		t.Run(string(mode), func(t *testing.T) {
			ci, ps, paths := expiryFixture(t)

			p := NewIntake(config.Config{
				Window: config.WindowConfig{Depth: 12 * time.Second, SafeEdgeBuffer: 3 * time.Second},
				Store:  config.StoreConfig{Cleanup: mode},
			}, "ch1", ci)

			p.expireOldSegments(ps)

			for i, path := range paths {
				if !exists(t, path) {
					t.Errorf("segment %d was deleted under cleanup mode %q", i+1, mode)
				}
			}
			// It must still have left the window in the index.
			_, _, expired := ps.AS("v0").Rep("r0", 16).StatusCounts()
			if expired != 1 {
				t.Errorf("expired=%d, want 1: the index should age out even when files are kept", expired)
			}
		})
	}
}

// Keeping every segment is the default, and it outranks the cleanup mode: a
// channel that will finish as VOD must not shed the history it is about to serve.
func TestExpireOldSegments_KeepAllSegmentsOutranksCleanup(t *testing.T) {
	ci, ps, paths := expiryFixture(t)

	p := NewIntake(config.Config{
		Window: config.WindowConfig{Depth: 12 * time.Second, SafeEdgeBuffer: 3 * time.Second},
		Store: config.StoreConfig{
			Cleanup:         config.CleanupOnExpire,
			KeepAllSegments: true,
		},
	}, "ch1", ci)

	p.expireOldSegments(ps)

	for i, path := range paths {
		if !exists(t, path) {
			t.Errorf("segment %d was deleted even though the channel keeps all segments", i+1)
		}
	}
	published, _, expired := ps.AS("v0").Rep("r0", 16).StatusCounts()
	if published != 3 || expired != 0 {
		t.Errorf("published=%d expired=%d, want 3 and 0: nothing should age out", published, expired)
	}
}

// A file already gone — removed by an operator, or by a previous run that
// crashed between deleting and recording it — is not an error worth failing on.
func TestExpireOldSegments_MissingFileIsNotFatal(t *testing.T) {
	ci, ps, paths := expiryFixture(t)
	if err := os.Remove(paths[0]); err != nil {
		t.Fatalf("pre-remove: %v", err)
	}

	p := NewIntake(config.Config{
		Window: config.WindowConfig{Depth: 12 * time.Second, SafeEdgeBuffer: 3 * time.Second},
		Store:  config.StoreConfig{Cleanup: config.CleanupOnExpire},
	}, "ch1", ci)

	p.expireOldSegments(ps) // must not panic or block

	_, _, expired := ps.AS("v0").Rep("r0", 16).StatusCounts()
	if expired != 1 {
		t.Errorf("expired=%d, want 1: the segment still has to leave the window", expired)
	}
}

// The whole of the expiry decision, with no index and no filesystem: given a
// live edge, which media time have segments aged out below, and should anything
// age out at all.
func TestExpiryCutoff(t *testing.T) {
	window := func(depth, safe time.Duration) config.WindowConfig {
		return config.WindowConfig{Depth: depth, SafeEdgeBuffer: safe}
	}

	tests := []struct {
		name       string
		cfg        config.Config
		liveEdge   float64
		wantCutoff float64
		wantOK     bool
	}{
		{
			name:     "live edge behind the window",
			cfg:      config.Config{Window: window(12*time.Second, 3*time.Second)},
			liveEdge: 30, wantCutoff: 15, wantOK: true,
		},
		{
			name:     "safe edge buffer widens the window",
			cfg:      config.Config{Window: window(12*time.Second, 10*time.Second)},
			liveEdge: 30, wantCutoff: 8, wantOK: true,
		},
		{
			// The default. Nothing ages out however the window is configured.
			name: "keeping all segments outranks the window",
			cfg: config.Config{
				Window: window(12*time.Second, 3*time.Second),
				Store:  config.StoreConfig{KeepAllSegments: true, Cleanup: config.CleanupOnExpire},
			},
			liveEdge: 30, wantOK: false,
		},
		{
			name:     "no window configured",
			cfg:      config.Config{Window: window(0, 3*time.Second)},
			liveEdge: 30, wantOK: false,
		},
		{
			// Nothing published yet: there is no live edge to measure back from,
			// and treating it as zero would put the cutoff in negative time.
			name:     "no live edge yet",
			cfg:      config.Config{Window: window(12*time.Second, 3*time.Second)},
			liveEdge: 0, wantOK: false,
		},
		{
			// A stream younger than its own window: everything is still inside it.
			name:     "window longer than the stream so far",
			cfg:      config.Config{Window: window(60*time.Second, 3*time.Second)},
			liveEdge: 30, wantOK: false,
		},
		{
			name:     "cutoff exactly at zero does not expire",
			cfg:      config.Config{Window: window(30*time.Second, 0)},
			liveEdge: 30, wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cutoff, ok := expiryCutoff(tc.cfg, tc.liveEdge)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (cutoff %v)", ok, tc.wantOK, cutoff)
			}
			if ok && cutoff != tc.wantCutoff {
				t.Errorf("cutoff = %v, want %v", cutoff, tc.wantCutoff)
			}
		})
	}
}
