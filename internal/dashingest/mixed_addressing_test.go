package dashingest

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/queue"
)

// mixedAddressingMPD is one AdaptationSet whose representation t0 uses a
// SegmentTemplate and b0 a SegmentBase over sbURL, followed by an audio
// AdaptationSet that must not share its init list.
func mixedAddressingMPD(sbURL string, idxStart, idxEnd uint64) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static"
     mediaPresentationDuration="PT4S" minBufferTime="PT2S">
  <Period id="p0" duration="PT4S">
    <AdaptationSet mimeType="video/mp4" contentType="video">
      <Representation id="t0" bandwidth="800000" codecs="avc1.64001f" width="640" height="360">
        <SegmentTemplate timescale="1000" duration="2000" startNumber="1"
                         initialization="t0/init.mp4" media="t0/$Number$.m4s"/>
      </Representation>
      <Representation id="b0" bandwidth="400000" codecs="avc1.64001f" width="320" height="180">
        <BaseURL>%s</BaseURL>
        <SegmentBase timescale="90000" indexRange="%d-%d">
          <Initialization range="0-%d"/>
        </SegmentBase>
      </Representation>
    </AdaptationSet>
    <AdaptationSet mimeType="audio/mp4" contentType="audio">
      <Representation id="a0" bandwidth="64000" codecs="mp4a.40.2">
        <SegmentTemplate timescale="1000" duration="2000" startNumber="1"
                         initialization="a0/init.mp4" media="a0/$Number$.m4s"/>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`, sbURL, idxStart, idxEnd, sbInitSize-1)
}

// Decrypting an AdaptationSet during ingest needs the init of every one of
// its representations, so every task of it must locate all of them, whatever
// addressing each representation uses. A list holding only the
// SegmentTemplate half lets that half decide the AdaptationSet alone.
func TestDiscoverTasksListEveryInitOfAMixedAdaptationSet(t *testing.T) {
	tests := []struct {
		name string
		// sidxReadable is false when b0's index cannot be fetched, so b0
		// has no task this pass; its init is still the AdaptationSet's.
		sidxReadable bool
	}{
		{name: "both halves have tasks", sidxReadable: true},
		{name: "SegmentBase index unreadable", sidxReadable: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			file, idxStart, idxEnd := segmentBaseFile(t, 1000, 2000)
			origin := startRangeOrigin(t, file, &rangeOrigin{})
			sbURL := origin.URL + "/b0.mp4"
			if !tc.sidxReadable {
				// The init region holds no sidx box.
				idxStart, idxEnd = 0, sbInitSize-1
			}
			p := parseMPD(t, mixedAddressingMPD(sbURL, idxStart, idxEnd))
			w := NewWatcher(config.Config{Upstream: config.UpstreamConfig{MPDURL: origin.URL + "/manifest.mpd"}},
				"ch", nil, index.NewChannelIndex(index.PublishAVBarrier), queue.NewBroker(0))

			tasks, _ := w.discoverTasks(context.Background(), p, nil, nil, nil)

			byRep := map[string]int{}
			for _, task := range tasks {
				byRep[task.RepID]++
				var got []string
				for _, ref := range task.ASInits {
					got = append(got, ref.RepID)
				}
				want := []string{"t0", "b0"}
				if task.RepID == "a0" {
					want = []string{"a0"}
				}
				if !slices.Equal(got, want) {
					t.Errorf("task %s/%d lists the inits of %v, want %v", task.RepID, task.SegNo, got, want)
				}
				for _, ref := range task.ASInits {
					if ref.RepID == "b0" && (ref.InitURL != sbURL || ref.InitRangeStart != 0 || ref.InitRangeEnd != sbInitSize-1) {
						t.Errorf("b0's init ref = %+v, want %s bytes 0-%d", ref, sbURL, sbInitSize-1)
					}
				}
			}
			wantB0 := 0
			if tc.sidxReadable {
				wantB0 = 2
			}
			if byRep["t0"] != 2 || byRep["b0"] != wantB0 || byRep["a0"] != 2 {
				t.Errorf("tasks per representation = %v, want t0:2 b0:%d a0:2", byRep, wantB0)
			}
		})
	}
}
