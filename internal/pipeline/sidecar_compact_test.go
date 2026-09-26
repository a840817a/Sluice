package pipeline

import (
	"testing"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/queue"
	"github.com/a840817a/sluice/internal/store"
)

// Compacting a sidecar keeps the record of every segment still on disk,
// whatever its format. A WebVTT segment's timing lives only in its record, so
// a compaction that dropped it would take the subtitle out of every rebuild.
func TestSidecarCompactionKeepsSegmentsStillOnDisk(t *testing.T) {
	for _, tc := range []struct{ mediaType, ext string }{
		{index.MediaVideo, store.ExtTS},
		{index.MediaVideo, store.ExtFMP4},
		{index.MediaText, store.ExtVTT},
	} {
		t.Run(tc.ext, func(t *testing.T) {
			dir := t.TempDir()
			p := NewProcessor(config.Config{Store: config.StoreConfig{DataDir: dir}}, "ch", index.NewChannelIndex(index.PublishAVBarrier), nil)
			task := &queue.SegmentTask{ChannelID: "ch", PeriodID: "0", MediaType: tc.mediaType, ASID: "0", RepID: "r0"}
			if err := store.Write(store.RepLocation{DataDir: dir, ChannelID: "ch", PeriodID: "0", MediaType: tc.mediaType, ASID: "0", RepID: "r0"}.SegmentPath(0, tc.ext), []byte("x")); err != nil {
				t.Fatal(err)
			}
			// Two records, then compact on the second: only segment 0 has a
			// file, segment 1 stands for one already expired off disk.
			p.persistSegmentMeta(task, index.SegmentState{SegNo: 0, EndPTS: 1, Timescale: 1})
			p.sidecarAppends[task.RepKey()] = sidecarCompactEvery - 1
			task.SegNo = 1
			p.persistSegmentMeta(task, index.SegmentState{SegNo: 1, EndPTS: 1, Timescale: 1})
			records, err := store.ReadSegmentMeta(store.RepLocation{DataDir: dir, ChannelID: "ch", PeriodID: "0", MediaType: tc.mediaType, ASID: "0", RepID: "r0"}.SidecarPath())
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 || records[0].SegNo != 0 {
				t.Errorf("sidecar after compaction holds %d records (%+v), want only segment 0's", len(records), records)
			}
		})
	}
}
