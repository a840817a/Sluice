package channel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocfg "github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/hlsingest"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/store"
)

func TestCheckpointVODMediaRepairsHLSSidecarsPerRepresentation(t *testing.T) {
	dir := t.TempDir()
	m := newTestManager(t, gocfg.Config{Store: gocfg.StoreConfig{DataDir: dir}})
	ci := index.NewChannelIndex(index.PublishAVBarrier)
	rt := &Runtime{ID: "channel", Index: ci, Source: hlsSource{w: &hlsingest.Watcher{}}}

	videoPath := store.RepLocation{DataDir: dir, ChannelID: rt.ID, PeriodID: "p0", MediaType: index.MediaVideo, ASID: "0", RepID: "v0"}.SegmentPath(7, store.ExtTS)
	audioPath := store.RepLocation{DataDir: dir, ChannelID: rt.ID, PeriodID: "p0", MediaType: index.MediaAudio, ASID: "1", RepID: "a0"}.SegmentPath(9, store.ExtTS)
	for _, path := range []string{videoPath, audioPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte("segment"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	ci.Period("p0").AS("0").Rep("v0", 8).RestorePublished(index.SegmentState{
		SegNo: 7, Path: videoPath, StartPTS: 100, EndPTS: 200, Timescale: 90_000,
	})
	ci.Period("p0").AS("1").Rep("a0", 8).RestorePublished(index.SegmentState{
		SegNo: 9, Path: audioPath, StartPTS: 300, EndPTS: 400, Timescale: 48_000,
		KeyURI: "https://keys.example.test/a", IV: []byte{1, 2, 3},
	})

	if w := m.checkpointVODMedia(rt); len(w) != 0 {
		t.Fatalf("checkpointVODMedia warnings: %q", w)
	}

	for _, tc := range []struct {
		path      string
		segNo     uint64
		timescale uint32
		keyURI    string
	}{
		{videoPath, 7, 90_000, ""},
		{audioPath, 9, 48_000, "https://keys.example.test/a"},
	} {
		records, err := store.ReadSegmentMeta(filepath.Join(filepath.Dir(tc.path), store.SidecarFileName))
		if err != nil {
			t.Fatalf("read sidecar for %s: %v", tc.path, err)
		}
		if len(records) != 1 {
			t.Fatalf("sidecar for %s has %d records, want 1", tc.path, len(records))
		}
		if got := records[0]; got.SegNo != tc.segNo || got.Timescale != tc.timescale || got.KeyURI != tc.keyURI {
			t.Errorf("sidecar for %s = %+v, want seg=%d timescale=%d key=%q", tc.path, got, tc.segNo, tc.timescale, tc.keyURI)
		}
	}
}

func TestCheckpointVODMediaReportsUnknownSidecarTimedSegment(t *testing.T) {
	// MPEG-TS and WebVTT both take their timing from the sidecar, so the
	// rebuild skips either with no record, and the checkpoint reports it
	// instead of refusing the whole VOD.
	for _, tc := range []struct{ mediaType, rep, ext string }{
		{index.MediaVideo, "v0", store.ExtTS},
		{index.MediaText, "s0", store.ExtVTT},
	} {
		t.Run(tc.ext, func(t *testing.T) {
			dir := t.TempDir()
			m := newTestManager(t, gocfg.Config{Store: gocfg.StoreConfig{DataDir: dir}})
			rt := &Runtime{ID: "channel", Index: index.NewChannelIndex(index.PublishAVBarrier), Source: hlsSource{w: &hlsingest.Watcher{}}}
			path := store.RepLocation{DataDir: dir, ChannelID: rt.ID, PeriodID: "p0", MediaType: tc.mediaType, ASID: "0", RepID: tc.rep}.SegmentPath(7, tc.ext)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("segment"), 0o644); err != nil {
				t.Fatal(err)
			}

			w := m.checkpointVODMedia(rt)
			if len(w) != 1 || !strings.Contains(w[0], "no restart metadata") {
				t.Fatalf("checkpointVODMedia warnings = %q, want one missing-metadata warning", w)
			}
		})
	}
}

func TestCheckpointVODMediaRetriesAfterBlockedSidecarPath(t *testing.T) {
	dir := t.TempDir()
	m := newTestManager(t, gocfg.Config{Store: gocfg.StoreConfig{DataDir: dir}})
	ci := index.NewChannelIndex(index.PublishAVBarrier)
	rt := &Runtime{ID: "channel", Index: ci, Source: hlsSource{w: &hlsingest.Watcher{}}}
	path := store.RepLocation{DataDir: dir, ChannelID: rt.ID, PeriodID: "p0", MediaType: index.MediaVideo, ASID: "0", RepID: "v0"}.SegmentPath(7, store.ExtTS)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("segment"), 0o644); err != nil {
		t.Fatal(err)
	}
	ci.Period("p0").AS("0").Rep("v0", 8).RestorePublished(index.SegmentState{
		SegNo: 7, Path: path, StartPTS: 100, EndPTS: 200, Timescale: 90_000,
	})

	sidecar := filepath.Join(filepath.Dir(path), store.SidecarFileName)
	if err := os.Mkdir(sidecar, 0o755); err != nil {
		t.Fatal(err)
	}
	if w := m.checkpointVODMedia(rt); len(w) == 0 {
		t.Fatal("checkpointVODMedia reported nothing with a sidecar directory blocking the write")
	}
	if err := os.Remove(sidecar); err != nil {
		t.Fatalf("remove blocked sidecar: %v", err)
	}
	if w := m.checkpointVODMedia(rt); len(w) != 0 {
		t.Fatalf("checkpointVODMedia retry warnings: %q", w)
	}
	records, err := store.ReadSegmentMeta(sidecar)
	if err != nil {
		t.Fatalf("read repaired sidecar: %v", err)
	}
	if len(records) != 1 || records[0].SegNo != 7 {
		t.Errorf("repaired sidecar = %+v, want segment 7", records)
	}
}

func TestCheckpointVODMediaAggregatesMissingMetadataPerDirectory(t *testing.T) {
	dir := t.TempDir()
	m := newTestManager(t, gocfg.Config{Store: gocfg.StoreConfig{DataDir: dir}})
	rt := &Runtime{ID: "channel", Index: index.NewChannelIndex(index.PublishAVBarrier), Source: hlsSource{w: &hlsingest.Watcher{}}}
	for n := uint64(1); n <= 500; n++ {
		path := store.RepLocation{DataDir: dir, ChannelID: rt.ID, PeriodID: "p0", MediaType: index.MediaVideo, ASID: "0", RepID: "v0"}.SegmentPath(n, store.ExtTS)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("segment"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A status poll carries every warning; a stray history must not flood it.
	w := m.checkpointVODMedia(rt)
	if len(w) != 1 || !strings.Contains(w[0], "500 segment(s)") {
		t.Fatalf("checkpointVODMedia warnings = %d entries, first %q; want one naming all 500", len(w), w)
	}
}
