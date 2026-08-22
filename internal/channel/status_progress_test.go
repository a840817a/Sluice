package channel

import (
	"encoding/json"
	"testing"

	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/progress"
	"github.com/a840817a/sluice/internal/queue"
)

// TestStoredSegmentsCountsEveryStatus pins the seed used when a channel restarts.
// Undercounting here makes a resumed channel look like it lost everything it had
// already downloaded; overcounting pushes its bar past 100%.
func TestStoredSegmentsCountsEveryStatus(t *testing.T) {
	ci := statusIndex(t)
	// statusIndex holds p0: 4 published + 1 committed + 1 expired, p1: 1 committed.
	if got := storedSegments(ci); got != 7 {
		t.Errorf("storedSegments = %d, want 7", got)
	}
	if got := storedSegments(index.NewChannelIndex()); got != 0 {
		t.Errorf("storedSegments(empty) = %d, want 0", got)
	}
}

func TestBuildStatusOmitsProgressWhenChannelHasNone(t *testing.T) {
	rt := &Runtime{ID: "ch1", Index: statusIndex(t)}
	rt.setConfig(Config{})

	s := buildStatus(rt)
	if s.Progress != nil {
		t.Errorf("Progress = %+v, want nil when the runtime has no counters", s.Progress)
	}

	// And it must marshal away entirely, not as "progress": null.
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["progress"]; ok {
		t.Error("progress key present in JSON, want omitted")
	}
}

// TestBuildProgressLiveChannelHasNoDenominator is the central assertion of this
// whole feature. The bug being fixed was a UI that manufactured a denominator for
// a live stream; the contract now is that a live channel reports a cumulative
// count and explicitly no total, so no percentage or ETA can be derived.
func TestBuildProgressLiveChannelHasNoDenominator(t *testing.T) {
	prog := progress.New()
	prog.AddStored(500, 5_000_000) // well past the 120-segment live ring capacity
	rt := &Runtime{ID: "ch1", Index: statusIndex(t), Progress: prog, Broker: queue.NewBroker(16)}
	rt.setConfig(Config{})

	p := buildStatus(rt).Progress
	if p == nil {
		t.Fatal("Progress is nil")
	}
	if p.SegmentsStored != 500 {
		t.Errorf("SegmentsStored = %d, want 500", p.SegmentsStored)
	}
	if p.TotalKnown {
		t.Error("TotalKnown = true for a live channel, want false")
	}
	if p.TotalSegments != 0 {
		t.Errorf("TotalSegments = %d, want 0", p.TotalSegments)
	}
	if p.ETASeconds != nil {
		t.Errorf("ETASeconds = %v, want nil for a live channel", *p.ETASeconds)
	}
	if p.Finalized {
		t.Error("Finalized = true for a running live channel")
	}
	// The cumulative count must not be capped by the DVR ring: that plateau is
	// exactly what the old display suffered from.
	if p.SegmentsStored <= 120 {
		t.Errorf("SegmentsStored = %d, want a cumulative count past the ring capacity", p.SegmentsStored)
	}
}

func TestBuildProgressStaticChannelHasTotalAndETA(t *testing.T) {
	prog := progress.New()
	prog.SetExpectedTotal(1000)
	prog.AddStored(400, 4_000_000)
	rt := &Runtime{ID: "ch1", Index: index.NewChannelIndex(), Progress: prog, Broker: queue.NewBroker(16)}
	rt.setConfig(Config{})

	p := buildStatus(rt).Progress
	if !p.TotalKnown || p.TotalSegments != 1000 {
		t.Errorf("TotalKnown=%v TotalSegments=%d, want true/1000", p.TotalKnown, p.TotalSegments)
	}
	if p.SegmentsStored != 400 {
		t.Errorf("SegmentsStored = %d, want 400", p.SegmentsStored)
	}
	if p.TotalCapped {
		t.Error("TotalCapped = true, want false")
	}
}

func TestBuildProgressCappedTotalSuppressesETA(t *testing.T) {
	prog := progress.New()
	prog.SetExpectedTotal(100_000)
	prog.MarkCapped()
	prog.AddStored(10, 1000)
	rt := &Runtime{ID: "ch1", Index: index.NewChannelIndex(), Progress: prog, Broker: queue.NewBroker(16)}
	rt.setConfig(Config{})

	p := buildStatus(rt).Progress
	if !p.TotalCapped {
		t.Error("TotalCapped = false, want true")
	}
	if p.ETASeconds != nil {
		t.Errorf("ETASeconds = %v, want nil — an ETA from a floor total is a lie", *p.ETASeconds)
	}
}

// TestBuildProgressReportsQueueAndDrops covers the two numbers that explain a
// download stuck below its total.
func TestBuildProgressReportsQueueAndDrops(t *testing.T) {
	b := queue.NewBroker(1)
	b.Push(&queue.SegmentTask{ChannelID: "ch", RepID: "r0", SegNo: 1, Priority: 0})
	b.Push(&queue.SegmentTask{ChannelID: "ch", RepID: "r0", SegNo: 2, Priority: 9}) // rejected

	rt := &Runtime{ID: "ch1", Index: index.NewChannelIndex(), Progress: progress.New(), Broker: b}
	rt.setConfig(Config{})

	p := buildStatus(rt).Progress
	if p.QueueDepth != 1 {
		t.Errorf("QueueDepth = %d, want 1", p.QueueDepth)
	}
	if p.SegmentsDropped != 1 {
		t.Errorf("SegmentsDropped = %d, want 1", p.SegmentsDropped)
	}
}

func TestBuildProgressSurfacesErrors(t *testing.T) {
	prog := progress.New()
	prog.RecordSegmentError("fetch", "connection reset by peer", "v0", 42)
	prog.SetSourceError("manifest", "502 Bad Gateway")
	rt := &Runtime{ID: "ch1", Index: index.NewChannelIndex(), Progress: prog, Broker: queue.NewBroker(16)}
	rt.setConfig(Config{})

	p := buildStatus(rt).Progress
	if p.LastSegmentError == nil || p.LastSegmentError.SegNo != 42 {
		t.Errorf("LastSegmentError = %+v, want seg 42", p.LastSegmentError)
	}
	if p.SourceError == nil || p.SourceError.Stage != "manifest" {
		t.Errorf("SourceError = %+v, want stage manifest", p.SourceError)
	}
	// A source failure is not a segment failure.
	if p.SegmentsFailed != 1 {
		t.Errorf("SegmentsFailed = %d, want 1", p.SegmentsFailed)
	}
}

// TestBuildTracksCoversEveryIndexedRep pins that the index, not the manifest,
// decides which tracks exist. A representation dropped upstream still has
// segments on disk and still serves, so hiding it would misreport the channel.
func TestBuildTracksCoversEveryIndexedRep(t *testing.T) {
	rt := &Runtime{ID: "ch1", Index: statusIndex(t)}
	rt.setConfig(Config{})

	tracks := buildStatus(rt).Tracks
	if len(tracks) != 3 {
		t.Fatalf("got %d tracks, want 3: %+v", len(tracks), tracks)
	}

	// Stable order, so a UI diffing rows every 2s does not see them shuffle.
	want := []struct{ period, as, rep string }{
		{"p0", "0", "v0"},
		{"p0", "1", "a0"},
		{"p1", "0", "v0"},
	}
	for i, w := range want {
		got := tracks[i]
		if got.PeriodID != w.period || got.ASID != w.as || got.RepID != w.rep {
			t.Errorf("tracks[%d] = %s/%s/%s, want %s/%s/%s",
				i, got.PeriodID, got.ASID, got.RepID, w.period, w.as, w.rep)
		}
	}

	// Counts and media types come through.
	if v := tracks[0]; v.MediaType != "video" || v.Published != 2 || v.Committed != 1 || v.MaxSegNo != 3 {
		t.Errorf("p0/0/v0 = %+v, want video 2 published, 1 committed, maxSegNo 3", v)
	}
	if a := tracks[1]; a.MediaType != "audio" || a.Published != 2 || a.Expired != 1 {
		t.Errorf("p0/1/a0 = %+v, want audio 2 published, 1 expired", a)
	}
}

func TestBuildTracksEmptyIndexYieldsNil(t *testing.T) {
	rt := &Runtime{ID: "ch1", Index: index.NewChannelIndex()}
	rt.setConfig(Config{})

	s := buildStatus(rt)
	if s.Tracks != nil {
		t.Errorf("Tracks = %+v, want nil for an empty index", s.Tracks)
	}
	raw, _ := json.Marshal(s)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	if _, ok := m["tracks"]; ok {
		t.Error("tracks key present in JSON for an empty index, want omitted")
	}
}

func TestDecorateDASHTracks(t *testing.T) {
	p := &imdp.ParsedMPD{
		Periods: []*imdp.ParsedPeriod{{
			ID: "p0",
			AdaptationSets: []*imdp.ParsedAdaptationSet{
				{
					ID: "0", Lang: "en",
					Representations: []*imdp.ParsedRepresentation{
						{ID: "v0", Bandwidth: 4_000_000, Width: 1920, Height: 1080, Codecs: "avc1.640028"},
					},
				},
				{
					ID: "1", Lang: "ja",
					Representations: []*imdp.ParsedRepresentation{
						{ID: "a0", Bandwidth: 128_000, Codecs: "mp4a.40.2"},
					},
				},
			},
		}},
	}

	tracks := []TrackStats{
		{PeriodID: "p0", ASID: "0", RepID: "v0", MediaType: "video"},
		{PeriodID: "p0", ASID: "1", RepID: "a0", MediaType: "audio"},
		// Present on disk but no longer in the manifest: must survive undecorated
		// rather than be dropped or crash the walk.
		{PeriodID: "p9", ASID: "0", RepID: "gone", MediaType: "video", Published: 5},
	}
	decorateDASHTracks(p, tracks)

	if v := tracks[0]; v.Width != 1920 || v.Height != 1080 || v.Bandwidth != 4_000_000 || v.Codecs != "avc1.640028" || v.Language != "en" {
		t.Errorf("video track = %+v", v)
	}
	if a := tracks[1]; a.Bandwidth != 128_000 || a.Codecs != "mp4a.40.2" || a.Language != "ja" {
		t.Errorf("audio track = %+v", a)
	}
	if g := tracks[2]; g.Codecs != "" || g.Published != 5 {
		t.Errorf("orphaned track = %+v, want undecorated with its counts intact", g)
	}
}

// TestDecorateDASHTracksKeysOnFullTriple guards a real ambiguity: a DASH RepID is
// only unique within its AdaptationSet, so two AdaptationSets can both contain a
// representation called "1". Keying on RepID alone would cross-contaminate them.
func TestDecorateDASHTracksKeysOnFullTriple(t *testing.T) {
	p := &imdp.ParsedMPD{
		Periods: []*imdp.ParsedPeriod{{
			ID: "p0",
			AdaptationSets: []*imdp.ParsedAdaptationSet{
				{ID: "0", Lang: "en", Representations: []*imdp.ParsedRepresentation{
					{ID: "1", Width: 1920, Height: 1080},
				}},
				{ID: "1", Lang: "de", Representations: []*imdp.ParsedRepresentation{
					{ID: "1", Bandwidth: 96_000},
				}},
			},
		}},
	}
	tracks := []TrackStats{
		{PeriodID: "p0", ASID: "0", RepID: "1"},
		{PeriodID: "p0", ASID: "1", RepID: "1"},
	}
	decorateDASHTracks(p, tracks)

	if tracks[0].Width != 1920 || tracks[0].Language != "en" {
		t.Errorf("AS 0 rep 1 = %+v, want the 1920x1080 English one", tracks[0])
	}
	if tracks[1].Width != 0 || tracks[1].Bandwidth != 96_000 || tracks[1].Language != "de" {
		t.Errorf("AS 1 rep 1 = %+v, want the 96kbps German one with no resolution", tracks[1])
	}
}

// TestPeriodStatsJSONIsUnchanged is a compatibility guard. The admin UI and any
// external consumer read these exact keys; the new progress reporting is additive
// and must not have renamed or dropped one.
func TestPeriodStatsJSONIsUnchanged(t *testing.T) {
	raw, err := json.Marshal(PeriodStats{PeriodID: "p0", Published: 1, Committed: 2, Expired: 3, Total: 6})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"period_id":"p0","published":1,"committed":2,"expired":3,"total":6}`
	if string(raw) != want {
		t.Errorf("PeriodStats JSON =\n  %s\nwant\n  %s", raw, want)
	}
}

func TestChannelStatusKeepsItsOriginalKeys(t *testing.T) {
	raw, err := json.Marshal(ChannelStatus{
		ID: "ch1", MPDURL: "http://x/live.mpd", Running: true, Mode: "live",
		Periods: []PeriodStats{{PeriodID: "p0"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"id", "mpd_url", "running", "mode", "periods"} {
		if _, ok := m[k]; !ok {
			t.Errorf("key %q missing from ChannelStatus JSON", k)
		}
	}
	if len(m) != 5 {
		t.Errorf("ChannelStatus has %d keys with no progress/tracks set, want 5: %v", len(m), m)
	}
}

func TestStatusForReturnsOnlyTheRequestedChannel(t *testing.T) {
	m := &Manager{runtimes: make(map[string]*Runtime)}

	rt1 := &Runtime{ID: "ch1", Index: statusIndex(t), Progress: progress.New()}
	rt1.setConfig(Config{MPDURL: "http://x/1.mpd"})
	rt2 := &Runtime{ID: "ch2", Index: index.NewChannelIndex(), Progress: progress.New()}
	rt2.setConfig(Config{MPDURL: "http://x/2.mpd"})
	m.runtimes["ch1"] = rt1
	m.runtimes["ch2"] = rt2

	got, ok := m.StatusFor("ch1")
	if !ok {
		t.Fatal("StatusFor(ch1) reported not running")
	}
	if got.ID != "ch1" || got.MPDURL != "http://x/1.mpd" {
		t.Errorf("StatusFor(ch1) = %+v", got)
	}
	if len(got.Tracks) != 3 {
		t.Errorf("got %d tracks, want 3", len(got.Tracks))
	}

	if _, ok := m.StatusFor("nope"); ok {
		t.Error("StatusFor of an unknown channel reported running")
	}
}
