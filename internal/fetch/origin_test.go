package fetch

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/queue"
)

// The channel's headers are read per request, so a rotation reaches the next
// one, and the gateway auth header is set after them, so it wins over a
// same-named channel header.
func TestOriginSetsChannelHeadersThenGatewayAuth(t *testing.T) {
	var got []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Clone())
	}))
	defer srv.Close()

	h := NewAtomicHeaders(map[string]string{"X-Token": "old", "Authorization": "Bearer channel"})
	o := NewOrigin(h, "Authorization", "Bearer gateway")
	if _, err := o.Get(t.Context(), srv.Client(), srv.URL, 10); err != nil {
		t.Fatal(err)
	}
	h.Store(map[string]string{"X-Token": "new"})
	if _, err := o.GetRange(t.Context(), srv.Client(), srv.URL, 0, 9, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := (*Origin)(nil).Get(t.Context(), srv.Client(), srv.URL, 10); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("origin saw %d requests, want 3", len(got))
	}
	for i, want := range []struct{ token, auth string }{{"old", "Bearer gateway"}, {"new", "Bearer gateway"}, {"", ""}} {
		if tok, auth := got[i].Get("X-Token"), got[i].Get("Authorization"); tok != want.token || auth != want.auth {
			t.Errorf("request %d: X-Token %q Authorization %q, want %q %q", i, tok, auth, want.token, want.auth)
		}
	}
	if r := got[1].Get("Range"); r != "bytes=0-9" {
		t.Errorf("range request sent Range %q", r)
	}
}

// A status the request does not accept is a StatusError carrying the code, so
// a caller can tell 410 Gone from 425 Too Early. A range accepts 200 as well
// as 206, as some servers answer a range with the whole file.
func TestOriginAcceptsOnlyItsStatuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		w.WriteHeader(code)
		w.Write([]byte("x"))
	}))
	defer srv.Close()
	o := NewOrigin(nil, "", "")
	for _, tc := range []struct {
		code      int
		byteRange bool
		wantErr   bool
	}{
		{200, false, false}, {206, false, true}, {410, false, true}, {425, false, true},
		{200, true, false}, {206, true, false}, {404, true, true},
	} {
		url := srv.URL + "/" + strconv.Itoa(tc.code)
		var err error
		if tc.byteRange {
			_, err = o.GetRange(t.Context(), srv.Client(), url, 0, 0, 0)
		} else {
			_, err = o.Get(t.Context(), srv.Client(), url, 10)
		}
		var se *StatusError
		switch {
		case !tc.wantErr && err != nil:
			t.Errorf("%d (range %v): %v", tc.code, tc.byteRange, err)
		case tc.wantErr && (!errors.As(err, &se) || se.Code != tc.code):
			t.Errorf("%d (range %v): err = %v, want a StatusError with code %d", tc.code, tc.byteRange, err, tc.code)
		}
	}
}

// Every read is bounded: a body over the limit is refused whether the origin
// declares its size or streams it, and a range reads no more than it asked
// for, so an origin that ignores Range is refused rather than buffered whole.
func TestOriginBoundsWhatItReads(t *testing.T) {
	body := strings.Repeat("x", 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/streamed" {
			w.Header().Set("Transfer-Encoding", "chunked")
			w.(http.Flusher).Flush()
		}
		w.Write([]byte(body)) // ignores any Range
	}))
	defer srv.Close()
	o := NewOrigin(nil, "", "")
	c := srv.Client()

	for _, path := range []string{"/declared", "/streamed"} {
		if _, err := o.Get(t.Context(), c, srv.URL+path, 99); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s over the limit: err = %v, want ErrTooLarge", path, err)
		}
		if data, err := o.Get(t.Context(), c, srv.URL+path, 100); err != nil || len(data) != 100 {
			t.Errorf("%s at the limit: %d bytes, %v", path, len(data), err)
		}
	}
	if _, err := o.GetRange(t.Context(), c, srv.URL+"/declared", 0, 9, 0); !errors.Is(err, ErrTooLarge) {
		t.Errorf("whole file for a 10-byte range: err = %v, want ErrTooLarge", err)
	}
	if _, err := o.GetRange(t.Context(), c, srv.URL+"/declared", 0, 199, 50); !errors.Is(err, ErrTooLarge) {
		t.Errorf("range above the cap: err = %v, want ErrTooLarge", err)
	}
	if data, err := o.GetRange(t.Context(), c, srv.URL+"/declared", 0, 199, 0); err != nil || len(data) != 100 {
		t.Errorf("range covering the file: %d bytes, %v", len(data), err)
	}
	if _, err := o.GetRange(t.Context(), c, srv.URL, 5, 4, 0); err == nil {
		t.Error("an inverted range was requested")
	}
}

// The pipeline drops a segment on 410 Gone and backs off briefly on 425 Too
// Early, so a fetch result must carry the origin's status for media and init
// requests alike, ranged or not.
func TestFetchResultCarriesTheOriginStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		w.WriteHeader(code)
	}))
	defer srv.Close()
	f := NewFetcher(nil, nil, 0, 0)
	for _, code := range []int{http.StatusGone, http.StatusTooEarly} {
		url := srv.URL + "/" + strconv.Itoa(code)
		for name, r := range map[string]Result{
			"segment":     f.Fetch(t.Context(), &queue.SegmentTask{URL: url}),
			"ranged":      f.Fetch(t.Context(), &queue.SegmentTask{URL: url, RangeEnd: 9}),
			"init":        f.FetchInit(t.Context(), &queue.SegmentTask{InitURL: url}),
			"ranged init": f.FetchInit(t.Context(), &queue.SegmentTask{InitURL: url, InitRangeEnd: 9}),
		} {
			if r.Err == nil || r.StatusCode != code {
				t.Errorf("%s answered %d: StatusCode %d, err %v", name, code, r.StatusCode, r.Err)
			}
		}
	}
}
