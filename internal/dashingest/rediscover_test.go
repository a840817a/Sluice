package dashingest

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/queue"
	"github.com/a840817a/sluice/internal/store"
)

// staticMPD is a static MPD whose one representation, v0 of AdaptationSet
// "0", has five segments.
func staticMPD(t *testing.T) *imdp.ParsedMPD {
	t.Helper()
	p, err := imdp.Parse([]byte(`<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT10S">
  <Period id="p0" start="PT0S" duration="PT10S">
    <AdaptationSet mimeType="video/mp4">
      <SegmentTemplate timescale="1" duration="2" startNumber="1" initialization="init.mp4" media="$Number$.m4s"/>
      <Representation id="v0" bandwidth="1"/>
      <Representation id="v1" bandwidth="2"/>
    </AdaptationSet>
  </Period>
</MPD>`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// waitQueued drains b until it has held want tasks, or a second passed.
func waitQueued(b *queue.Broker, want int) map[string][]uint64 {
	out := map[string][]uint64{}
	n := 0
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline) && n < want; {
		t := b.Pop()
		if t == nil {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		out[t.RepID] = append(out[t.RepID], t.SegNo)
		t.Finish()
		n++
	}
	return out
}

// A static MPD is read once, so Rediscover enumerates it again for the
// representations it names, offering the segments not on disk, while the
// Run that read it is not cancelled.
func TestRediscoverStatic(t *testing.T) {
	dataDir := t.TempDir()
	broker := queue.NewBroker(100)
	w := NewWatcher(config.Config{Store: config.StoreConfig{DataDir: dataDir}}, "ch", nil,
		index.NewChannelIndex(index.PublishImmediately), broker)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.runCtx = ctx
	w.processMPD(ctx, staticMPD(t))
	if got := waitQueued(broker, 10); len(got["v0"]) != 5 || len(got["v1"]) != 5 {
		t.Fatalf("first pass queued %v, want five segments of each representation", got)
	}
	// Segments 3 and 5 of v0 were dropped unstored.
	loc := store.RepLocation{DataDir: dataDir, ChannelID: "ch", PeriodID: "p0", MediaType: "video", ASID: "0", RepID: "v0"}
	for _, n := range []uint64{1, 2, 4} {
		if err := store.Write(loc.SegmentPath(n, store.ExtFMP4), []byte("x")); err != nil {
			t.Fatal(err)
		}
	}

	w.Rediscover([]string{queue.RepKey("ch", "p0", "0", "v0")})
	got := waitQueued(broker, 2)
	if !slices.Equal(got["v0"], []uint64{3, 5}) || len(got) != 1 {
		t.Errorf("Rediscover queued %v, want v0's segments 3 and 5", got)
	}
	if extra := waitQueued(broker, 1); len(extra) != 0 {
		t.Errorf("Rediscover queued more: %v", extra)
	}

	cancel()
	w.Rediscover([]string{queue.RepKey("ch", "p0", "0", "v0")})
	if got := waitQueued(broker, 1); len(got) != 0 {
		t.Errorf("Rediscover after the Run was cancelled queued %v", got)
	}
}

// Every poll of a dynamic MPD offers again a segment it lists below the
// highest one the index tracks that is not stored, however far back it lies
// in the MPD's window. Rediscover makes the next poll offer those above it.
func TestRediscoverDynamicCoversTheWindow(t *testing.T) {
	ci := index.NewChannelIndex(index.PublishImmediately)
	w, broker := newRestartWatcher(ci)
	mpd := func() *imdp.ParsedMPD { return liveMPD(t, ast0, 10, 8) }
	w.processMPD(context.Background(), mpd())
	if got := drain(broker); len(got["p0"]) != 8 {
		t.Fatalf("first poll queued %v, want 8 segments", got)
	}
	// 10 and 13 dropped unstored, as are 16 and 17 above the highest stored.
	for _, n := range []uint64{11, 12, 14, 15} {
		ci.Commit(index.RepRef{PeriodID: "p0", ASID: "0", RepID: "v0", MediaType: "video"}, 100,
			index.SegmentState{SegNo: n, StartPTS: int64(n) * 2, EndPTS: int64(n)*2 + 2, Timescale: 1})
	}
	w.processMPD(context.Background(), mpd())
	if got := drain(broker); !slices.Equal(got["p0"], []uint64{10, 13}) {
		t.Errorf("next poll queued %v, want 10 and 13", got["p0"])
	}
	w.Rediscover([]string{queue.RepKey("ch", "p0", "0", "v0")})
	w.processMPD(context.Background(), mpd())
	got := drain(broker)["p0"]
	slices.Sort(got)
	if !slices.Equal(got, []uint64{10, 13, 16, 17}) {
		t.Errorf("poll after Rediscover queued %v, want 10, 13, 16 and 17", got)
	}
}
