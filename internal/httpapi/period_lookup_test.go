package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/gwurl"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/store"
)

// periodRouter serves the gateway's public routes for a manager.
func periodRouter(t *testing.T, cfg config.Config, mgr *ch.Manager) http.Handler {
	t.Helper()
	st, err := ch.NewStore(filepath.Join(cfg.Store.DataDir, "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	return NewRouter(cfg, NewServer(cfg, mgr), NewAdminServer(context.Background(), mgr, st), UI{})
}

func get(t *testing.T, h http.Handler, path string) (int, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	body, _ := io.ReadAll(rec.Body)
	return rec.Code, body
}

// Two periods of one channel both hold segment 5 of representation v0, with
// different content: what an upstream reset that rewinds the media sequence
// leaves on disk. Each period's segment and init must be reachable, and each
// URL must name exactly one file.
func TestSegmentAndInitURLsNameOnePeriod(t *testing.T) {
	dataDir := t.TempDir()
	const id = "reset"
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	segs, inits := map[string][]byte{}, map[string][]byte{}
	for i, period := range []string{"0", "1"} {
		inits[period] = fmp4Init(uint32(cmafTimescale + i))
		write(store.InitPath(dataDir, id, period, "video", "0", "v0"), inits[period])
		segs[period] = fmp4Segment(uint64(i+1) * 1000 * cmafTimescale)
		write(store.SegmentPath(dataDir, id, period, "video", "0", "v0", 5), segs[period])
	}

	cfg := config.Config{Store: config.StoreConfig{DataDir: dataDir}, Server: config.ServerConfig{BaseURL: "http://gw.test"}}
	mgr := ch.NewManager(cfg)
	t.Cleanup(mgr.StopAll)
	if _, err := mgr.Start(context.Background(), id, ch.Config{MPDURL: "http://origin.invalid/a.mpd", IsVOD: true}); err != nil {
		t.Fatal(err)
	}
	h := periodRouter(t, cfg, mgr)

	for _, period := range []string{"0", "1"} {
		for i := 0; i < 20; i++ {
			code, body := get(t, h, "/v1/channels/"+id+"/segments/"+period+"/v0/5.m4s")
			if code != http.StatusOK || !bytes.Equal(body, segs[period]) {
				t.Fatalf("period %s segment 5: status %d, right file %v", period, code, bytes.Equal(body, segs[period]))
			}
			code, body = get(t, h, "/v1/channels/"+id+"/init/"+period+"/v0.mp4")
			if code != http.StatusOK || !bytes.Equal(body, inits[period]) {
				t.Fatalf("period %s init: status %d, right file %v", period, code, bytes.Equal(body, inits[period]))
			}
		}
	}
	// The period-less URLs could only guess which period was meant.
	for _, path := range []string{"/v1/channels/" + id + "/segments/v0/5.m4s", "/v1/channels/" + id + "/init/v0.mp4"} {
		if code, _ := get(t, h, path); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
}

// resettingCMAFOrigin serves a live fMP4 stream whose media sequence runs
// 100..104 until reset is set, and then restarts at 0..2 — an encoder restart.
// Segment content encodes which stream and number it is, so a test can tell
// exactly which file a URL returned.
func resettingCMAFOrigin(t *testing.T, reset *atomic.Bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-INDEPENDENT-SEGMENTS\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS=\"avc1.42c01e\"\n"+
			"media/v0.m3u8\n")
	})
	mux.HandleFunc("/media/v0.m3u8", func(w http.ResponseWriter, r *http.Request) {
		first, last, stream := 100, 104, "a"
		if reset.Load() {
			first, last, stream = 0, 2, "b"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", first)
		b.WriteString(`#EXT-X-MAP:URI="init.mp4"` + "\n")
		for n := first; n <= last; n++ {
			fmt.Fprintf(&b, "#EXTINF:1.000,\n%s%d.m4s\n", stream, n)
		}
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/media/init.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.Write(fmp4Init(cmafTimescale))
	})
	mux.HandleFunc("/media/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(resetSegment(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/media/"), ".m4s")))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// resetSegment is the content of origin segment name ("a101", "b0", ...).
func resetSegment(name string) []byte {
	n, _ := strconv.Atoi(name[1:])
	base := uint64(n) * cmafTimescale
	if name[0] == 'b' {
		base += 1_000_000 * cmafTimescale
	}
	return fmp4Segment(base)
}

var playlistURI = regexp.MustCompile(`(?m)^http://gw\.test(/\S+)$`)

// An upstream reset starts a new period whose numbers rewind below the old
// ones. The live playlist must move on to the new stream after a
// discontinuity, keep media sequence numbers increasing, and every URL it
// lists must return the segment it stands for.
func TestLivePlaylistFollowsUpstreamReset(t *testing.T) {
	var reset atomic.Bool
	origin := resettingCMAFOrigin(t, &reset)
	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: t.TempDir()},
		Worker: config.WorkerConfig{FetchWorkers: 2},
		Window: config.WindowConfig{Depth: time.Hour},
	}
	mgr := ch.NewManager(cfg)
	t.Cleanup(mgr.StopAll)
	if _, err := mgr.Start(context.Background(), "live", ch.Config{
		SourceType: ch.SourceHLS, MPDURL: origin.URL + "/master.m3u8", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	h := periodRouter(t, cfg, mgr)
	playlist := func() string {
		_, body := get(t, h, "/v1/channels/live/media/v0.m3u8")
		return string(body)
	}
	waitFor := func(what string, ok func(string) bool) string {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			p := playlist()
			if ok(p) {
				return p
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s:\n%s", what, p)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	before := waitFor("the first stream", func(p string) bool { return strings.Count(p, "#EXTINF:") >= 5 })
	beforeSeq := mediaSequence(t, before)
	reset.Store(true)

	after := waitFor("the stream after the reset", func(p string) bool {
		return strings.Count(p, "#EXTINF:") >= 8 && strings.Contains(p, "#EXT-X-DISCONTINUITY\n")
	})
	if seq := mediaSequence(t, after); seq < beforeSeq {
		t.Errorf("media sequence went backwards across the reset: %d then %d", beforeSeq, seq)
	}

	// Walk the playlist in order: stream a 100..104, a discontinuity, then
	// stream b 0..2, each URL returning exactly that segment.
	want := []string{"a100", "a101", "a102", "a103", "a104", "b0", "b1", "b2"}
	var segURIs []string
	for _, m := range playlistURI.FindAllStringSubmatch(after, -1) {
		if !strings.Contains(m[1], "/init/") {
			segURIs = append(segURIs, m[1])
		}
	}
	if len(segURIs) != len(want) {
		t.Fatalf("playlist lists %d segments, want %d:\n%s", len(segURIs), len(want), after)
	}
	for i, uri := range segURIs {
		code, body := get(t, h, uri)
		if code != http.StatusOK || !bytes.Equal(body, resetSegment(want[i])) {
			t.Errorf("entry %d (%s): status %d, want origin segment %s", i, uri, code, want[i])
		}
	}
	if i := strings.Index(after, "#EXT-X-DISCONTINUITY\n"); i < 0 || !strings.Contains(after[i:], segURIs[5]) || strings.Contains(after[i:], segURIs[4]) {
		t.Errorf("the discontinuity is not between the two streams:\n%s", after)
	}
}

func mediaSequence(t *testing.T, playlist string) int {
	t.Helper()
	m := regexp.MustCompile(`#EXT-X-MEDIA-SEQUENCE:(\d+)`).FindStringSubmatch(playlist)
	if m == nil {
		t.Fatalf("no media sequence in:\n%s", playlist)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// DASH period IDs are arbitrary upstream strings. One that needs encoding must
// still reach its own files through the real router, whichever form of the
// path segment the router hands the handler.
func TestUnusualPeriodIDReachesItsSegment(t *testing.T) {
	dataDir := t.TempDir()
	const id = "odd"
	periods := []string{"p 1", "%", "a$b"}
	want := map[string][]byte{}
	for i, period := range periods {
		want[period] = fmp4Segment(uint64(i+1) * 1000 * cmafTimescale)
		path := store.SegmentPath(dataDir, id, period, "video", "0", "v0", 5)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want[period], 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{Store: config.StoreConfig{DataDir: dataDir}, Server: config.ServerConfig{BaseURL: "http://gw.test"}}
	mgr := ch.NewManager(cfg)
	t.Cleanup(mgr.StopAll)
	if _, err := mgr.Start(context.Background(), id, ch.Config{MPDURL: "http://origin.invalid/a.mpd", IsVOD: true}); err != nil {
		t.Fatal(err)
	}
	h := periodRouter(t, cfg, mgr)
	for _, period := range periods {
		uri := strings.TrimPrefix(gwurl.SegmentURL(cfg.Server.BaseURL, id, period, "v0", 5, ".m4s"), cfg.Server.BaseURL)
		if code, body := get(t, h, uri); code != http.StatusOK || !bytes.Equal(body, want[period]) {
			t.Errorf("period %q via %s: status %d, right file %v", period, uri, code, bytes.Equal(body, want[period]))
		}
	}
}

// The playlist numbers a later period after an earlier one's lifetime range,
// so the handler must pass that range on rather than let it be recomputed
// from whatever the ring still holds, and must order epochs numerically.
func TestPeriodSegmentsCarryLifetimeRangeInOrder(t *testing.T) {
	ci := index.NewChannelIndex()
	for _, period := range []string{"10", "2"} {
		rep := ci.Period(period).AS("0").Rep("v0", 2) // holds 2; 5 pushed
		for n := uint64(100); n <= 104; n++ {
			rep.Commit(index.SegmentState{SegNo: n, EndPTS: 1, Timescale: 1})
		}
	}
	srv := NewServer(config.Config{}, ch.NewManager(config.Config{}))
	got := srv.periodSegmentsForRep(ch.Serving{Index: ci}, "c", "v0")
	if len(got) != 2 || got[0].PeriodID != "2" || got[1].PeriodID != "10" {
		t.Fatalf("periods = %+v, want 2 then 10", got)
	}
	for _, p := range got {
		if !p.RangeKnown || p.First != 100 || p.Last != 104 {
			t.Errorf("period %s range = %d..%d (known %v), want the lifetime 100..104", p.PeriodID, p.First, p.Last, p.RangeKnown)
		}
	}
}
