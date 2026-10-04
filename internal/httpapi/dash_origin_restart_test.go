package httpapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
)

// restartSegment is an fMP4 segment starting at bmdt whose mdat names the
// stream it came from, so a test can tell which one the gateway served.
func restartSegment(bmdt uint64, stream byte) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, bmdt)
	moof := mp4box("moof", mp4box("traf", mp4box("tfdt", append([]byte{1, 0, 0, 0}, b...))))
	return append(moof, mp4box("mdat", []byte{'S', stream})...)
}

// A live DASH origin that restarts its numbering inside its Period must not
// have its two streams spliced into one period: the playlist plays the old
// stream to its end, then the restarted one after a discontinuity, each in
// its own period and in time order (ADR-0007).
func TestDASHOriginRestartStartsANewPeriod(t *testing.T) {
	var restarted atomic.Bool
	ast := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	stream := func() (start, count int, name byte) {
		if restarted.Load() {
			return 1, 8, 'B'
		}
		return 10, 5, 'A'
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest.mpd", func(w http.ResponseWriter, r *http.Request) {
		start, count, _ := stream()
		fmt.Fprintf(w, `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic" availabilityStartTime="%s" minimumUpdatePeriod="PT1S" timeShiftBufferDepth="PT60S" minBufferTime="PT2S">
  <Period id="p0" start="PT0S">
    <AdaptationSet mimeType="video/mp4">
      <SegmentTemplate timescale="90000" startNumber="%d" initialization="init.mp4" media="$Number$.m4s">
        <SegmentTimeline><S t="0" d="180000" r="%d"/></SegmentTimeline>
      </SegmentTemplate>
      <Representation id="v0" bandwidth="1000000" codecs="avc1.4d401f" width="1280" height="720"/>
    </AdaptationSet>
  </Period>
</MPD>`, ast, start, count-1)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/init.mp4" {
			w.Write(fmp4Init(90000))
			return
		}
		start, count, name := stream()
		var n int
		if _, err := fmt.Sscanf(r.URL.Path, "/%d.m4s", &n); err != nil || n < start || n >= start+count {
			http.NotFound(w, r)
			return
		}
		w.Write(restartSegment(uint64(n-start)*180000, name))
	})
	origin := httptest.NewServer(mux)
	t.Cleanup(origin.Close)

	cfg := config.Config{
		Server:   config.ServerConfig{BaseURL: "http://gw.test"},
		Store:    config.StoreConfig{DataDir: t.TempDir()},
		Upstream: config.UpstreamConfig{PollInterval: 100 * time.Millisecond},
		Worker:   config.WorkerConfig{FetchWorkers: 2, MaxRetriesStatic: 2},
		Window:   config.WindowConfig{Depth: time.Hour},
	}
	mgr := newTestManager(t, cfg)
	if _, err := createChannel(mgr, context.Background(), "dr", ch.Config{MPDURL: origin.URL + "/manifest.mpd", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Stop("dr") })
	srv := NewServer(cfg, mgr)
	playlist := func() string {
		return executeRequest(srv.handleMediaPlaylist, http.MethodGet, "/",
			map[string]string{"channelID": "dr", "repID": "v0"}).Body.String()
	}
	waitFor := func(segments int) string {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			pl := playlist()
			if strings.Count(pl, "#EXTINF") >= segments {
				return pl
			}
			if time.Now().After(deadline) {
				t.Fatalf("playlist never listed %d segments:\n%s", segments, pl)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	waitFor(5)
	restarted.Store(true)
	pl := waitFor(13)

	type served struct {
		period string
		stream byte
		tfdt   uint64
	}
	var got []served
	for _, m := range regexp.MustCompile(`/segments/([^/]+)/v0/(\d+)\.m4s`).FindAllStringSubmatch(pl, -1) {
		body := executeRequest(srv.handleSegment, http.MethodGet, "/",
			map[string]string{"channelID": "dr", "periodID": m[1], "repID": "v0", "segFile": m[2] + ".m4s"}).Body.Bytes()
		i, j := bytes.Index(body, []byte("tfdt")), bytes.LastIndex(body, []byte("S"))
		if i < 0 || j < 0 || j+1 >= len(body) {
			t.Fatalf("segment %s/%s is not one the origin served", m[1], m[2])
		}
		got = append(got, served{m[1], body[j+1], binary.BigEndian.Uint64(body[i+8 : i+16])})
	}

	var summary []string
	for _, s := range got {
		summary = append(summary, fmt.Sprintf("%s:%c@%d", s.period, s.stream, s.tfdt))
	}
	for i, s := range got {
		want := byte('A')
		if i >= 5 {
			want = 'B'
		}
		if s.stream != want {
			t.Fatalf("playlist plays %v, want the old stream's 5 segments, then the restarted stream's 8", summary)
		}
		if i > 0 && got[i-1].period == s.period && s.tfdt <= got[i-1].tfdt {
			t.Fatalf("time runs backwards inside period %s: %v", s.period, summary)
		}
	}
	if got[0].period == got[5].period {
		t.Errorf("both streams share period %s: %v", got[0].period, summary)
	}
	if !strings.Contains(pl, "#EXT-X-DISCONTINUITY") {
		t.Errorf("no discontinuity between the streams:\n%s", pl)
	}
}
