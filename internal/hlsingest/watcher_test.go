package hlsingest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/hlskey"
	"github.com/a840817a/sluice/internal/hlssrc"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/queue"
)

func newTestWatcher() *Watcher {
	return NewWatcher(
		config.Config{},
		"ch1", "https://origin.example.com/live/master.m3u8", nil, false, "",
		index.NewChannelIndex(),
		queue.NewBroker(4096),
	)
}

// drain pops every task the broker holds, keyed by segment number.
func drain(b *queue.Broker) map[uint64]*queue.SegmentTask {
	out := make(map[uint64]*queue.SegmentTask)
	for {
		t := b.Pop()
		if t == nil {
			return out
		}
		out[t.SegNo] = t
	}
}

// mediaPlaylist builds a Media with n segments of durSec each, starting at
// media sequence startSeq.
func mediaPlaylist(startSeq uint64, n int, durSec float64) *hlssrc.Media {
	m := &hlssrc.Media{TargetDuration: int(durSec) + 1, MediaSequence: startSeq}
	for i := 0; i < n; i++ {
		m.Segments = append(m.Segments, hlssrc.Segment{
			URI:      "https://origin.example.com/live/s.ts",
			Duration: durSec,
			SeqNo:    startSeq + uint64(i),
		})
	}
	return m
}

func testVariant() variant {
	return variant{Variant: hlsdesc.Variant{RepID: "v0"}, url: "https://origin.example.com/live/v0.m3u8"}
}

func TestEnqueueNewAccumulatesPTSFromEXTINF(t *testing.T) {
	w := newTestWatcher()
	st := &variantState{}

	added := w.enqueueNew(testVariant(), st, mediaPlaylist(100, 3, 6.0))
	if added != 3 {
		t.Fatalf("added = %d, want 3", added)
	}

	tasks := drain(w.broker)
	const tick = 6.0 * TSTimescale // 540000 ticks for a 6 s segment

	for i, segNo := range []uint64{100, 101, 102} {
		task, ok := tasks[segNo]
		if !ok {
			t.Fatalf("segment %d not enqueued", segNo)
		}
		if task.Format != queue.FormatTS {
			t.Errorf("seg %d Format = %v, want FormatTS", segNo, task.Format)
		}
		if task.Timescale != TSTimescale {
			t.Errorf("seg %d Timescale = %d, want %d", segNo, task.Timescale, TSTimescale)
		}
		if want := int64(i) * tick; task.StartPTS != want {
			t.Errorf("seg %d StartPTS = %d, want %d", segNo, task.StartPTS, want)
		}
		if task.Duration != uint64(tick) {
			t.Errorf("seg %d Duration = %d, want %d", segNo, task.Duration, uint64(tick))
		}
	}
}

func TestEnqueueNewRoundTripsFractionalEXTINF(t *testing.T) {
	// EXTINF must survive the trip through 90 kHz ticks, since the outbound
	// playlist re-derives it from StartPTS/EndPTS.
	w := newTestWatcher()
	st := &variantState{}
	w.enqueueNew(testVariant(), st, mediaPlaylist(0, 1, 5.005))

	task := drain(w.broker)[0]
	got := float64(task.Duration) / TSTimescale
	if diff := got - 5.005; diff > 1e-4 || diff < -1e-4 {
		t.Errorf("round-tripped EXTINF = %v, want 5.005", got)
	}
}

func TestEnqueueNewDedupesAcrossReloads(t *testing.T) {
	w := newTestWatcher()
	st := &variantState{}
	v := testVariant()

	if added := w.enqueueNew(v, st, mediaPlaylist(100, 3, 6.0)); added != 3 {
		t.Fatalf("first reload added %d, want 3", added)
	}
	drain(w.broker)

	// The window slid by one: 101,102 are repeats and only 103 is new.
	if added := w.enqueueNew(v, st, mediaPlaylist(101, 3, 6.0)); added != 1 {
		t.Fatalf("second reload added %d, want 1", added)
	}
	tasks := drain(w.broker)
	if _, ok := tasks[103]; !ok || len(tasks) != 1 {
		t.Fatalf("expected only segment 103, got %v", tasks)
	}
	// The accumulator must continue rather than restart.
	if want := int64(3 * 6.0 * TSTimescale); tasks[103].StartPTS != want {
		t.Errorf("StartPTS = %d, want %d", tasks[103].StartPTS, want)
	}
}

func TestEnqueueNewIdenticalReloadAddsNothing(t *testing.T) {
	w := newTestWatcher()
	st := &variantState{}
	v := testVariant()

	w.enqueueNew(v, st, mediaPlaylist(100, 3, 6.0))
	drain(w.broker)

	if added := w.enqueueNew(v, st, mediaPlaylist(100, 3, 6.0)); added != 0 {
		t.Errorf("unchanged playlist added %d segments, want 0", added)
	}
}

func TestEnqueueNewTreatsSequenceRewindAsReset(t *testing.T) {
	// An upstream encoder restart rewinds EXT-X-MEDIA-SEQUENCE. Segment 5 after
	// the restart has nothing to do with segment 5 before it, so the channel
	// must move to a new period instead of silently deduplicating the new
	// segments away — that is what would stall ingest indefinitely.
	w := newTestWatcher()
	st := &variantState{}
	v := testVariant()

	w.enqueueNew(v, st, mediaPlaylist(500, 3, 6.0))
	tasks := drain(w.broker)
	if tasks[500].PeriodID != "0" {
		t.Fatalf("initial PeriodID = %q, want 0", tasks[500].PeriodID)
	}

	added := w.enqueueNew(v, st, mediaPlaylist(0, 3, 6.0))
	if added != 3 {
		t.Fatalf("after reset added %d, want 3", added)
	}
	tasks = drain(w.broker)
	if len(tasks) != 3 {
		t.Fatalf("after reset got %d tasks, want 3", len(tasks))
	}
	if got := tasks[0].PeriodID; got != "1" {
		t.Errorf("PeriodID after reset = %q, want 1", got)
	}
	// The timeline restarts with the new period.
	if tasks[0].StartPTS != 0 {
		t.Errorf("StartPTS after reset = %d, want 0", tasks[0].StartPTS)
	}
	if w.PeriodID() != "1" {
		t.Errorf("watcher PeriodID = %q, want 1", w.PeriodID())
	}
}

func TestEnqueueNewSurvivesWindowJump(t *testing.T) {
	// The gateway fell behind and the upstream window moved past it. Ingest
	// must resume at the new window rather than refuse the segments.
	w := newTestWatcher()
	st := &variantState{}
	v := testVariant()

	w.enqueueNew(v, st, mediaPlaylist(100, 3, 6.0))
	drain(w.broker)

	added := w.enqueueNew(v, st, mediaPlaylist(200, 3, 6.0))
	if added != 3 {
		t.Fatalf("added = %d after a forward jump, want 3", added)
	}
	tasks := drain(w.broker)
	if _, ok := tasks[200]; !ok {
		t.Error("segment 200 not enqueued after the jump")
	}
	// A forward jump is a gap, not a reset: the period must not change.
	if tasks[200].PeriodID != "0" {
		t.Errorf("PeriodID = %q, want 0 (forward gap is not a reset)", tasks[200].PeriodID)
	}
}

func TestEnqueueNewCarriesDiscontinuity(t *testing.T) {
	w := newTestWatcher()
	st := &variantState{}
	m := mediaPlaylist(10, 3, 6.0)
	m.Segments[1].Discontinuity = true

	w.enqueueNew(testVariant(), st, m)
	tasks := drain(w.broker)

	if !tasks[11].Discontinuity {
		t.Error("discontinuity not carried onto the task")
	}
	if tasks[10].Discontinuity || tasks[12].Discontinuity {
		t.Error("discontinuity leaked onto neighbouring segments")
	}
}

func TestEnqueueNewDetectsFMP4ViaMap(t *testing.T) {
	w := newTestWatcher()
	st := &variantState{}
	m := mediaPlaylist(0, 1, 4.0)
	m.Segments[0].Map = &hlssrc.MapInfo{URI: "https://origin.example.com/live/init.mp4"}

	w.enqueueNew(testVariant(), st, m)
	task := drain(w.broker)[0]

	if task.Format != queue.FormatFMP4 {
		t.Errorf("Format = %v, want FormatFMP4 when EXT-X-MAP is present", task.Format)
	}
	if task.InitURL != "https://origin.example.com/live/init.mp4" {
		t.Errorf("InitURL = %q", task.InitURL)
	}
	if !w.IsFMP4() {
		t.Error("watcher did not record the source as fMP4")
	}
}

func TestSegmentPriorityFavoursOlderSegments(t *testing.T) {
	// Older segments are closest to falling out of the upstream window, so they
	// must be fetched first. Lower priority value = more urgent.
	if segmentPriority(100) >= segmentPriority(101) {
		t.Error("older segment must have the more urgent priority")
	}
	// Must not overflow into a negative priority on a long-running stream.
	if p := segmentPriority(1 << 40); p < 0 {
		t.Errorf("priority overflowed to %d", p)
	}
}

func TestDiscoverMasterPlaylist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#EXTM3U\n" +
			"#EXT-X-INDEPENDENT-SEGMENTS\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=1280000,RESOLUTION=1280x720,CODECS=\"avc1.4d401f\"\n" +
			"v720.m3u8\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=640000,RESOLUTION=640x360\n" +
			"v360.m3u8\n"))
	}))
	defer srv.Close()

	w := NewWatcher(config.Config{}, "ch1", srv.URL+"/master.m3u8", nil, false, "",
		index.NewChannelIndex(), queue.NewBroker(16))

	if err := w.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}
	vs := w.Variants()
	if len(vs) != 2 {
		t.Fatalf("len(Variants) = %d, want 2", len(vs))
	}
	if vs[0].RepID != "v0" || vs[1].RepID != "v1" {
		t.Errorf("rep IDs = %q,%q, want v0,v1", vs[0].RepID, vs[1].RepID)
	}
	if vs[0].Bandwidth != 1280000 || vs[0].Width != 1280 {
		t.Errorf("variant 0 = %+v", vs[0])
	}
	if !w.IndependentSegments() {
		t.Error("IndependentSegments not recorded")
	}
	if !w.Ready() {
		t.Error("watcher should be ready after discovery")
	}
}

func TestDiscoverBareMediaPlaylist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.0,\ns0.ts\n"))
	}))
	defer srv.Close()

	w := NewWatcher(config.Config{}, "ch1", srv.URL+"/index.m3u8", nil, false, "",
		index.NewChannelIndex(), queue.NewBroker(16))

	if err := w.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}
	vs := w.Variants()
	if len(vs) != 1 || vs[0].RepID != "v0" {
		t.Fatalf("Variants = %+v, want a single v0", vs)
	}
	// EXT-X-STREAM-INF requires BANDWIDTH, so a placeholder must be supplied.
	if vs[0].Bandwidth == 0 {
		t.Error("bare media playlist must still get a non-zero bandwidth")
	}
}

func TestDiscoverRejectsNonPlaylist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>not a playlist</html>"))
	}))
	defer srv.Close()

	w := NewWatcher(config.Config{}, "ch1", srv.URL+"/index.m3u8", nil, false, "",
		index.NewChannelIndex(), queue.NewBroker(16))

	if err := w.discover(context.Background()); err == nil {
		t.Error("discover accepted a non-playlist response")
	}
}

func TestDiscoverPropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	w := NewWatcher(config.Config{}, "ch1", srv.URL+"/index.m3u8", nil, false, "",
		index.NewChannelIndex(), queue.NewBroker(16))

	if err := w.discover(context.Background()); err == nil {
		t.Error("discover ignored a 404 response")
	}
}

func encryptedPlaylist(startSeq uint64, n int, keyURI, ivHex string) *hlssrc.Media {
	m := mediaPlaylist(startSeq, n, 4.0)
	k := &hlssrc.Key{Method: "AES-128", URI: keyURI}
	if ivHex != "" {
		iv := make([]byte, 16)
		for i := 0; i < 16; i++ {
			var b int
			fmt.Sscanf(ivHex[i*2:i*2+2], "%02x", &b)
			iv[i] = byte(b)
		}
		k.IV = iv
	}
	for i := range m.Segments {
		m.Segments[i].Key = k
	}
	return m
}

func TestEnqueueNewCarriesKeyMaterial(t *testing.T) {
	w := newTestWatcher()
	st := &variantState{}
	const keyURI = "https://drm.example.com/k1.bin"
	ivHex := "0123456789abcdef0123456789abcdef"

	w.enqueueNew(testVariant(), st, encryptedPlaylist(5, 2, keyURI, ivHex))
	tasks := drain(w.broker)

	task := tasks[5]
	if !task.Encrypted {
		t.Fatal("task not marked encrypted")
	}
	if task.KeyURI != keyURI {
		t.Errorf("KeyURI = %q, want %q", task.KeyURI, keyURI)
	}
	if len(task.IV) != 16 {
		t.Errorf("IV is %d bytes, want 16", len(task.IV))
	}
	if task.IV[0] != 0x01 || task.IV[15] != 0xef {
		t.Errorf("IV = %x, want the value from the tag", task.IV)
	}
	// Passthrough is the default, so nothing is decrypted during ingest.
	if task.DecryptOnIngest {
		t.Error("DecryptOnIngest set on a passthrough channel")
	}

	// The key URI must be resolvable through the registry the proxy uses.
	if uri, ok := w.KeyURIForID(hlskey.KeyID(keyURI)); !ok || uri != keyURI {
		t.Errorf("KeyURIForID = (%q, %v), want the registered URI", uri, ok)
	}
}

func TestEnqueueNewDerivesIVFromSequenceWhenAbsent(t *testing.T) {
	w := newTestWatcher()
	st := &variantState{}
	w.enqueueNew(testVariant(), st, encryptedPlaylist(258, 1, "https://drm.example.com/k.bin", ""))

	task := drain(w.broker)[258]
	if len(task.IV) != 16 {
		t.Fatalf("IV is %d bytes, want 16", len(task.IV))
	}
	// 258 = 0x0102 in the low bytes, big-endian (RFC 8216 §5.2).
	if task.IV[14] != 0x01 || task.IV[15] != 0x02 {
		t.Errorf("derived IV = %x, want the media sequence number", task.IV)
	}
}

func TestEnqueueNewHonoursDecryptMode(t *testing.T) {
	w := NewWatcher(config.Config{}, "ch1", "https://o.example.com/m.m3u8", nil,
		true, "", index.NewChannelIndex(), queue.NewBroker(64))
	st := &variantState{}
	w.enqueueNew(testVariant(), st, encryptedPlaylist(0, 1, "https://drm.example.com/k.bin", ""))

	if task := drain(w.broker)[0]; !task.DecryptOnIngest {
		t.Error("DecryptOnIngest not set on a decrypt-mode channel")
	}
}

func TestEnqueueNewIgnoresSampleAES(t *testing.T) {
	// SAMPLE-AES (FairPlay) is out of scope: the segment is passed through and
	// must not be treated as something we can decrypt.
	w := newTestWatcher()
	st := &variantState{}
	m := mediaPlaylist(0, 1, 4.0)
	m.Segments[0].Key = &hlssrc.Key{Method: "SAMPLE-AES", URI: "https://drm.example.com/fp"}

	w.enqueueNew(testVariant(), st, m)
	if task := drain(w.broker)[0]; task.Encrypted {
		t.Error("SAMPLE-AES segment marked as AES-128 encrypted")
	}
}

func TestKeyURIForIDRejectsUnknownID(t *testing.T) {
	// The proxy must never resolve an ID it did not see in a playlist, or the
	// gateway becomes an open relay.
	w := newTestWatcher()
	if _, ok := w.KeyURIForID(hlskey.KeyID("https://evil.example.com/steal")); ok {
		t.Error("resolved a key ID that was never registered")
	}
}

// commitToIndex mimics the processor: it commits each drained task to the index
// as a published segment, so a later poll sees them as already held.
func commitToIndex(w *Watcher, tasks map[uint64]*queue.SegmentTask) {
	for _, task := range tasks {
		ps := w.ci.Period(task.PeriodID)
		as := ps.AS(task.ASID)
		as.SetMediaType(task.MediaType)
		rep := as.Rep(task.RepID, 0)
		rep.Commit(index.SegmentState{
			SegNo:     task.SegNo,
			StartPTS:  task.StartPTS,
			EndPTS:    task.StartPTS + int64(task.Duration),
			Timescale: task.Timescale,
		})
		rep.MarkPublished(map[uint64]struct{}{task.SegNo: {}})
	}
}

func TestEnqueueNewBackfillsInteriorGap(t *testing.T) {
	w := newTestWatcher()
	st := &variantState{}
	v := testVariant()

	// First poll: 100,101,102 all enqueue and commit to the index.
	w.enqueueNew(v, st, mediaPlaylist(100, 3, 6.0))
	commitToIndex(w, drain(w.broker))

	// Segment 103 was lost (never committed): simulate by committing only 104
	// from the next window, leaving a hole at 103.
	w.enqueueNew(v, st, mediaPlaylist(101, 4, 6.0)) // 101..104; 103 among them
	tasks := drain(w.broker)
	// 103 and 104 are new (>lastSeq 102); commit only 104, dropping 103.
	delete(tasks, 103)
	commitToIndex(w, tasks)

	// A later poll still advertises 102..105. 103 is now an interior hole (below
	// the high-water mark, still in the window) and must be re-enqueued.
	added := w.enqueueNew(v, st, mediaPlaylist(102, 4, 6.0)) // 102,103,104,105
	got := drain(w.broker)

	if _, ok := got[103]; !ok {
		t.Errorf("interior gap 103 was not backfilled; got %v", keysOf(got))
	}
	if _, ok := got[105]; !ok {
		t.Errorf("new segment 105 not enqueued; got %v", keysOf(got))
	}
	// Segments already held (102, 104) must not be re-enqueued.
	if _, ok := got[102]; ok {
		t.Error("already-held segment 102 was re-enqueued")
	}
	if _, ok := got[104]; ok {
		t.Error("already-held segment 104 was re-enqueued")
	}
	if added != len(got) {
		t.Errorf("added=%d but drained %d", added, len(got))
	}

	// The backfilled hole must land at its true timeline position (right after
	// 102), not at the live edge.
	if want := int64(3 * 6.0 * TSTimescale); got[103].StartPTS != want {
		t.Errorf("backfilled 103 StartPTS = %d, want %d (contiguous after 102)", got[103].StartPTS, want)
	}
}

func TestEnqueueNewSkipsGapWithoutAnchor(t *testing.T) {
	// A hole whose predecessor is not held cannot be placed on the timeline, so
	// it is left for a later poll rather than fetched with a wrong StartPTS.
	w := newTestWatcher()
	st := &variantState{}
	v := testVariant()

	w.enqueueNew(v, st, mediaPlaylist(100, 3, 6.0)) // 100,101,102
	tasks := drain(w.broker)
	// Commit only 102, so 100 and 101 are holes with no committed predecessor.
	for k := range tasks {
		if k != 102 {
			delete(tasks, k)
		}
	}
	commitToIndex(w, tasks)

	// Re-poll the same window; 100 has no anchor (99 absent) so it is skipped,
	// 101's anchor 100 is also absent so it is skipped too.
	w.enqueueNew(v, st, mediaPlaylist(100, 3, 6.0))
	got := drain(w.broker)
	if _, ok := got[100]; ok {
		t.Error("gap 100 backfilled despite no anchor")
	}
}

func keysOf(m map[uint64]*queue.SegmentTask) []uint64 {
	out := make([]uint64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestDiscoverDemuxedAudioRenditions(t *testing.T) {
	master := "#EXTM3U\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aac\",NAME=\"English\",LANGUAGE=\"en\",DEFAULT=YES,URI=\"audio/en.m3u8\"\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aac\",NAME=\"日本語\",LANGUAGE=\"ja\",URI=\"audio/ja.m3u8\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1280000,RESOLUTION=1280x720,CODECS=\"avc1.4d401f\",AUDIO=\"aac\"\n" +
		"v720/index.m3u8\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(master))
	}))
	defer srv.Close()

	w := NewWatcher(config.Config{}, "ch1", srv.URL+"/master.m3u8", nil, false, "",
		index.NewChannelIndex(), queue.NewBroker(16))
	if err := w.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}

	// Video variant carries its audio group.
	vs := w.Variants()
	if len(vs) != 1 || vs[0].AudioGroup != "aac" {
		t.Fatalf("variants = %+v, want one bound to AUDIO group aac", vs)
	}
	// Two demuxed audio renditions become separate poll targets.
	rs := w.Renditions()
	if len(rs) != 2 {
		t.Fatalf("len(Renditions) = %d, want 2", len(rs))
	}
	if rs[0].RepID != "a0" || rs[0].Language != "en" || !rs[0].Default {
		t.Errorf("rendition 0 = %+v", rs[0])
	}
	if rs[1].RepID != "a1" || rs[1].Language != "ja" || rs[1].Name != "日本語" {
		t.Errorf("rendition 1 = %+v", rs[1])
	}
}

func TestDiscoverMuxedAudioRenditionIgnored(t *testing.T) {
	// An EXT-X-MEDIA with no URI is muxed into the variant; it must not become a
	// separate poll target.
	master := "#EXTM3U\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aac\",NAME=\"English\",DEFAULT=YES\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1280000,AUDIO=\"aac\"\nv0.m3u8\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(master))
	}))
	defer srv.Close()

	w := NewWatcher(config.Config{}, "ch1", srv.URL+"/master.m3u8", nil, false, "",
		index.NewChannelIndex(), queue.NewBroker(16))
	if err := w.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if n := len(w.Renditions()); n != 0 {
		t.Errorf("muxed audio produced %d renditions, want 0", n)
	}
}

func TestSourceStateRoundTripsRenditions(t *testing.T) {
	// A restarted channel (which may never re-run discovery) must reconstruct
	// its audio renditions and per-target AdaptationSet ids from hls-source.json.
	dir := t.TempDir()
	statePath := filepath.Join(dir, "hls-source.json")

	master := "#EXTM3U\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",LANGUAGE="en",DEFAULT=YES,URI="audio/en.m3u8"` + "\n" +
		`#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=640x360,CODECS="avc1.4d401e",AUDIO="aud"` + "\n" +
		"v0/index.m3u8\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(master))
	}))
	defer srv.Close()

	// First process: discover writes the state file.
	w1 := NewWatcher(config.Config{}, "ch1", srv.URL+"/master.m3u8", nil, false, statePath,
		index.NewChannelIndex(), queue.NewBroker(16))
	if err := w1.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}

	// Second process (restart): loads state on construction, without discovery.
	w2 := NewWatcher(config.Config{}, "ch1", srv.URL+"/master.m3u8", nil, false, statePath,
		index.NewChannelIndex(), queue.NewBroker(16))

	vs := w2.Variants()
	if len(vs) != 1 || vs[0].RepID != "v0" || vs[0].AudioGroup != "aud" {
		t.Fatalf("restored variants = %+v, want v0 bound to aud", vs)
	}
	rs := w2.Renditions()
	if len(rs) != 1 || rs[0].RepID != "a0" || rs[0].GroupID != "aud" || rs[0].Language != "en" || !rs[0].Default {
		t.Fatalf("restored renditions = %+v", rs)
	}
	// The audio target must reload into the audio AdaptationSet, not video's.
	for _, v := range w2.variants {
		if v.rendition && v.asID != AudioASID {
			t.Errorf("restored audio rendition asID = %q, want %q", v.asID, AudioASID)
		}
		if !v.rendition && v.asID != hlsASID {
			t.Errorf("restored video variant asID = %q, want %q", v.asID, hlsASID)
		}
	}
}

func TestDiscoverDemuxedSubtitles(t *testing.T) {
	master := "#EXTM3U\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",URI="audio/en.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",DEFAULT=YES,URI="subs/en.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="Forced",LANGUAGE="en",FORCED=YES,URI="subs/forced.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="Muxed"` + "\n" + // no URI: must be ignored
		`#EXT-X-STREAM-INF:BANDWIDTH=900000,CODECS="avc1.4d401e",AUDIO="aud",SUBTITLES="subs"` + "\n" +
		"v0.m3u8\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(master))
	}))
	defer srv.Close()

	w := NewWatcher(config.Config{}, "ch1", srv.URL+"/master.m3u8", nil, false, "",
		index.NewChannelIndex(), queue.NewBroker(16))
	if err := w.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}

	// The variant binds both audio and subtitle groups.
	if v := w.Variants(); len(v) != 1 || v[0].AudioGroup != "aud" || v[0].SubtitleGroup != "subs" {
		t.Fatalf("variant = %+v, want bound to aud+subs", v)
	}
	// Two subtitle renditions (the URI-less one is ignored); audio stays separate.
	subs := w.Subtitles()
	if len(subs) != 2 {
		t.Fatalf("len(Subtitles) = %d, want 2 (URI-less ignored)", len(subs))
	}
	if subs[0].RepID != "s0" || subs[0].Language != "en" || !subs[0].Default {
		t.Errorf("subtitle 0 = %+v", subs[0])
	}
	if subs[1].RepID != "s1" || !subs[1].Forced {
		t.Errorf("subtitle 1 = %+v", subs[1])
	}
	if len(w.Renditions()) != 1 {
		t.Errorf("audio renditions = %d, want 1 (subtitles must not leak in)", len(w.Renditions()))
	}
}
