package dashingest

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	mp4 "github.com/abema/go-mp4"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/queue"
)

// A SegmentBase representation is one file on the origin. Discovery reads its
// sidx over a Range request and turns each referenced subsegment into a task
// addressed by byte offset, so nothing here is exercised without an origin that
// honours Range.

// sbInitSize is how many bytes of the single file the init (moov) occupies;
// the sidx follows it.
const sbInitSize = 200

// segmentBaseFile builds a single-file representation: init bytes, then a real
// sidx describing sizes, then filler standing in for the media.
func segmentBaseFile(t *testing.T, sizes ...uint32) (file []byte, idxStart, idxEnd uint64) {
	t.Helper()

	refs := make([]mp4.SidxReference, 0, len(sizes))
	for _, s := range sizes {
		refs = append(refs, mp4.SidxReference{
			ReferencedSize: s, SubsegmentDuration: 90000, StartsWithSAP: true, SAPType: 1,
		})
	}
	sidx := &mp4.Sidx{Timescale: 90000, ReferenceCount: uint16(len(refs)), References: refs}

	var payload bytes.Buffer
	if _, err := mp4.Marshal(&payload, sidx, mp4.Context{}); err != nil {
		t.Fatalf("marshal sidx: %v", err)
	}
	box := binary.BigEndian.AppendUint32(nil, uint32(8+payload.Len()))
	box = append(box, 's', 'i', 'd', 'x')
	box = append(box, payload.Bytes()...)

	file = append(file, bytes.Repeat([]byte{'I'}, sbInitSize)...)
	idxStart = uint64(len(file))
	file = append(file, box...)
	idxEnd = uint64(len(file)) - 1

	var media int
	for _, s := range sizes {
		media += int(s)
	}
	file = append(file, bytes.Repeat([]byte{'M'}, media)...)
	return file, idxStart, idxEnd
}

// rangeOrigin serves one file and honours Range, recording what was asked for.
type rangeOrigin struct {
	mu     sync.Mutex
	ranges []string
}

func startRangeOrigin(t *testing.T, file []byte, rec *rangeOrigin) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Range")
		rec.mu.Lock()
		rec.ranges = append(rec.ranges, hdr)
		rec.mu.Unlock()

		var start, end uint64
		if _, err := fmt.Sscanf(hdr, "bytes=%d-%d", &start, &end); err != nil {
			w.Write(file)
			return
		}
		if end >= uint64(len(file)) {
			end = uint64(len(file)) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(file)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(file[start : end+1])
	}))
	t.Cleanup(srv.Close)
	return srv
}

func segmentBaseWatcher(t *testing.T) *Watcher {
	t.Helper()
	return NewWatcher(config.Config{}, "ch", nil, index.NewChannelIndex(index.PublishAVBarrier), queue.NewBroker(0))
}

func parseMPD(t *testing.T, raw string) *imdp.ParsedMPD {
	t.Helper()
	p, err := imdp.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse MPD: %v", err)
	}
	return p
}

func segmentBaseMPDFor(fileURL string, idxStart, idxEnd uint64) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static"
     mediaPresentationDuration="PT12S" minBufferTime="PT2S"
     profiles="urn:mpeg:dash:profile:isoff-on-demand:2011">
  <Period id="p0">
    <AdaptationSet id="1" mimeType="video/mp4" contentType="video">
      <Representation id="v0" bandwidth="800000" codecs="avc1.64001f" width="640" height="360">
        <BaseURL>%s</BaseURL>
        <SegmentBase timescale="90000" indexRange="%d-%d">
          <Initialization range="0-%d"/>
        </SegmentBase>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`, fileURL, idxStart, idxEnd, sbInitSize-1)
}

// The whole path: fetch the sidx over Range, parse it, and turn each reference
// into a task pointing at the right bytes of the same file.
func TestDiscoverSegmentBaseTasksAddressesEachSubsegment(t *testing.T) {
	file, idxStart, idxEnd := segmentBaseFile(t, 1000, 2000, 1500)
	rec := &rangeOrigin{}
	origin := startRangeOrigin(t, file, rec)

	p := parseMPD(t, segmentBaseMPDFor(origin.URL+"/video.mp4", idxStart, idxEnd))
	w := segmentBaseWatcher(t)

	tasks := w.discoverSegmentBaseTasks(context.Background(), p, origin.URL, nil, nil, nil)

	if len(tasks) != 3 {
		t.Fatalf("got %d tasks, want one per sidx reference (3)", len(tasks))
	}

	// Media starts at the first byte after the index region.
	mediaStart := idxEnd + 1
	want := []struct{ start, end uint64 }{
		{mediaStart, mediaStart + 999},
		{mediaStart + 1000, mediaStart + 2999},
		{mediaStart + 3000, mediaStart + 4499},
	}
	for i, task := range tasks {
		if task.SegNo != uint64(i+1) {
			t.Errorf("task %d SegNo = %d, want %d (1-based)", i, task.SegNo, i+1)
		}
		if task.RangeStart != want[i].start || task.RangeEnd != want[i].end {
			t.Errorf("task %d range = %d-%d, want %d-%d",
				i, task.RangeStart, task.RangeEnd, want[i].start, want[i].end)
		}
		if !strings.HasSuffix(task.URL, "/video.mp4") {
			t.Errorf("task %d URL = %q, want the single file the ranges address", i, task.URL)
		}
	}

	// The sidx must have been fetched as a range, not by pulling the whole file.
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.ranges) == 0 {
		t.Fatal("the origin was never asked for anything")
	}
	wantRange := fmt.Sprintf("bytes=%d-%d", idxStart, idxEnd)
	if rec.ranges[0] != wantRange {
		t.Errorf("first request Range = %q, want %q — the index must be fetched by range",
			rec.ranges[0], wantRange)
	}
}

// The init is a byte range at the head of the same file, and every task has to
// carry it or the fetched media has no moov to be parsed against.
func TestDiscoverSegmentBaseTasksCarryTheInitRange(t *testing.T) {
	file, idxStart, idxEnd := segmentBaseFile(t, 800, 900)
	origin := startRangeOrigin(t, file, &rangeOrigin{})

	p := parseMPD(t, segmentBaseMPDFor(origin.URL+"/video.mp4", idxStart, idxEnd))
	tasks := segmentBaseWatcher(t).discoverSegmentBaseTasks(
		context.Background(), p, origin.URL, nil, nil, nil)

	if len(tasks) == 0 {
		t.Fatal("no tasks discovered")
	}
	for i, task := range tasks {
		if task.InitRangeStart != 0 || task.InitRangeEnd != sbInitSize-1 {
			t.Errorf("task %d init range = %d-%d, want 0-%d",
				i, task.InitRangeStart, task.InitRangeEnd, sbInitSize-1)
		}
	}
}

// A representation whose index cannot be read is skipped, not fatal: the
// watcher keeps serving the representations that do work.
func TestDiscoverSegmentBaseTasksSkipsUnreadableRepresentations(t *testing.T) {
	tests := []struct {
		name string
		// mpdBase is where the manifest itself was fetched from. A rep with no
		// BaseURL of its own resolves against it, so only an empty one leaves a
		// representation with nothing to address.
		emptyMPDBase bool
		mpd          func(originURL string, idxStart, idxEnd uint64) string
	}{
		{
			name: "malformed indexRange",
			mpd: func(originURL string, _, _ uint64) string {
				return segmentBaseMPDFor(originURL+"/video.mp4", 0, 0)[:0] +
					strings.Replace(segmentBaseMPDFor(originURL+"/video.mp4", 0, 0),
						`indexRange="0-0"`, `indexRange="not-a-range"`, 1)
			},
		},
		{
			name: "index range points at bytes that are not a sidx",
			mpd: func(originURL string, _, _ uint64) string {
				// The init region: real bytes, but no sidx box in them.
				return segmentBaseMPDFor(originURL+"/video.mp4", 0, sbInitSize-1)
			},
		},
		{
			name:         "nothing to resolve the file against",
			emptyMPDBase: true,
			mpd: func(_ string, idxStart, idxEnd uint64) string {
				return strings.Replace(
					segmentBaseMPDFor("", idxStart, idxEnd),
					"<BaseURL></BaseURL>", "", 1)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			file, idxStart, idxEnd := segmentBaseFile(t, 500)
			origin := startRangeOrigin(t, file, &rangeOrigin{})

			mpdBase := origin.URL
			if tc.emptyMPDBase {
				mpdBase = ""
			}
			p := parseMPD(t, tc.mpd(origin.URL, idxStart, idxEnd))
			tasks := segmentBaseWatcher(t).discoverSegmentBaseTasks(
				context.Background(), p, mpdBase, nil, nil, nil)

			if len(tasks) != 0 {
				t.Errorf("got %d tasks, want none: this representation is not addressable", len(tasks))
			}
		})
	}
}

// A representation with no SegmentBase at all is simply not ours to discover
// here — SegmentTemplate and SegmentList go through their own path.
func TestDiscoverSegmentBaseTasksIgnoresOtherAddressingModes(t *testing.T) {
	const raw = `<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static"
     mediaPresentationDuration="PT12S" minBufferTime="PT2S">
  <Period id="p0" duration="PT12S">
    <AdaptationSet id="1" mimeType="video/mp4" contentType="video">
      <SegmentTemplate timescale="1000" duration="2000" startNumber="1"
                       initialization="init.mp4" media="v0_$Number%09d$.m4s"/>
      <Representation id="v0" bandwidth="800000" codecs="avc1.64001f" width="640" height="360"/>
    </AdaptationSet>
  </Period>
</MPD>`

	p := parseMPD(t, raw)
	tasks := segmentBaseWatcher(t).discoverSegmentBaseTasks(
		context.Background(), p, "http://origin", nil, nil, nil)

	if len(tasks) != 0 {
		t.Errorf("got %d tasks from a SegmentTemplate representation, want none", len(tasks))
	}
}
