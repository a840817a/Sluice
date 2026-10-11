package httpapi

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
)

// dashPeriodSummary is what a DASH manifest says about one period: where it
// starts and how many segments it names.
type dashPeriodSummary struct {
	ID, Start string
	Segments  int
}

// summarizeDASH reads the periods of a served manifest (SegmentTimeline) or a
// local one (SegmentList).
func summarizeDASH(t *testing.T, data []byte) (duration string, periods []dashPeriodSummary) {
	t.Helper()
	var doc struct {
		Duration string `xml:"mediaPresentationDuration,attr"`
		Periods  []struct {
			ID    string `xml:"id,attr"`
			Start string `xml:"start,attr"`
			Reps  []struct {
				Timeline []struct {
					R int `xml:"r,attr"`
				} `xml:"SegmentTemplate>SegmentTimeline>S"`
				URLs []struct{} `xml:"SegmentList>SegmentURL"`
			} `xml:"AdaptationSet>Representation"`
		} `xml:"Period"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse manifest: %v\n%s", err, data)
	}
	for _, p := range doc.Periods {
		s := dashPeriodSummary{ID: p.ID, Start: p.Start}
		for _, r := range p.Reps {
			for _, tl := range r.Timeline {
				s.Segments += tl.R + 1
			}
			s.Segments += len(r.URLs)
		}
		periods = append(periods, s)
	}
	return doc.Duration, periods
}

// A DASH VOD names every period the gateway stored, not only those the
// origin's last MPD lists (ADR-0008): the period an origin restart ended
// (ADR-0007), and one that rolled off the MPD before ingest finished. The
// served manifest, the local one and the manifest a restarted gateway serves
// all lay the two periods out one after the other.
func TestDASHVODKeepsPeriodsTheLastMPDDoesNotList(t *testing.T) {
	for _, tc := range []struct {
		name string
		// second is the Period the origin lists after the switch.
		second, secondStart string
		wantIDs             [2]string
	}{
		{name: "origin_restart", second: "p0", secondStart: "PT0S", wantIDs: [2]string{"p0", "p0.r1"}},
		{name: "period_left_the_mpd", second: "p1", secondStart: "PT10S", wantIDs: [2]string{"p0", "p1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var switched atomic.Bool
			ast := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
			// Before the switch the origin offers segments 10..14 of p0; after
			// it, segments 1..8 of the second period, timed from zero again.
			stream := func() (period, start string, first, count int) {
				if switched.Load() {
					return tc.second, tc.secondStart, 1, 8
				}
				return "p0", "PT0S", 10, 5
			}
			mux := http.NewServeMux()
			mux.HandleFunc("/manifest.mpd", func(w http.ResponseWriter, r *http.Request) {
				period, start, first, count := stream()
				fmt.Fprintf(w, `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic" availabilityStartTime="%s" minimumUpdatePeriod="PT1S" timeShiftBufferDepth="PT60S" minBufferTime="PT2S">
  <Period id="%s" start="%s">
    <AdaptationSet mimeType="video/mp4">
      <SegmentTemplate timescale="90000" startNumber="%d" initialization="init.mp4" media="$Number$.m4s">
        <SegmentTimeline><S t="0" d="180000" r="%d"/></SegmentTimeline>
      </SegmentTemplate>
      <Representation id="v0" bandwidth="1000000" codecs="avc1.4d401f" width="1280" height="720"/>
    </AdaptationSet>
  </Period>
</MPD>`, ast, period, start, first, count-1)
			})
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/init.mp4" {
					w.Write(fmp4Init(90000))
					return
				}
				_, _, first, count := stream()
				var n int
				if _, err := fmt.Sscanf(r.URL.Path, "/%d.m4s", &n); err != nil || n < first || n >= first+count {
					http.NotFound(w, r)
					return
				}
				w.Write(restartSegment(uint64(n-first)*180000, 'S'))
			})
			origin := httptest.NewServer(mux)
			t.Cleanup(origin.Close)

			dataDir := t.TempDir()
			const id = "periods"
			h1 := boot(t, dataDir)
			t.Cleanup(h1.mgr.StopAll)
			if err := h1.store.Put(id, ch.Config{MPDURL: origin.URL + "/manifest.mpd", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			h1.start(t)
			h1.waitForSegments(t, id, 5)
			switched.Store(true)
			h1.waitForSegments(t, id, 13)

			done := make(chan struct{})
			h1.mgr.SetOnVODReady(func(*ch.Runtime) { close(done) })
			if err := h1.mgr.TransitionToVOD(id); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatalf("VOD transition did not finish (mode %v)", h1.mgr.Get(id).Mode())
			}
			if info, _ := h1.mgr.Channel(id); len(info.Status.VODWarnings) > 0 {
				t.Errorf("vod_warnings = %q, want none", info.Status.VODWarnings)
			}

			check := func(what string, data []byte) {
				t.Helper()
				duration, periods := summarizeDASH(t, data)
				want := []dashPeriodSummary{
					{ID: tc.wantIDs[0], Start: "PT0S", Segments: 5},
					{ID: tc.wantIDs[1], Start: "PT10S", Segments: 8},
				}
				for i := range periods {
					// The local manifest writes fractional seconds.
					periods[i].Start = strings.Replace(periods[i].Start, ".000000S", "S", 1)
				}
				if fmt.Sprint(periods) != fmt.Sprint(want) {
					t.Errorf("%s periods = %v, want %v", what, periods, want)
				}
				if d := strings.Replace(duration, ".000S", "S", 1); d != "PT26S" {
					t.Errorf("%s mediaPresentationDuration = %s, want PT26S", what, duration)
				}
			}
			served := executeRequest(h1.srv.handleManifest, http.MethodGet, "/", map[string]string{"channelID": id})
			check("served manifest", served.Body.Bytes())
			for _, file := range []string{"manifest.mpd", "local.mpd"} {
				data, err := os.ReadFile(filepath.Join(dataDir, id, file))
				if err != nil {
					t.Fatal(err)
				}
				check(file, data)
			}

			h1.stop(id)
			origin.Close()
			h2 := boot(t, dataDir)
			t.Cleanup(h2.mgr.StopAll)
			h2.start(t)
			restored := executeRequest(h2.srv.handleManifest, http.MethodGet, "/", map[string]string{"channelID": id})
			check("manifest after restart", restored.Body.Bytes())
		})
	}
}
