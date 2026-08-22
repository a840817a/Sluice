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

func TestReadAtMost(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		limit   int64
		want    string
		wantErr bool
	}{
		{name: "under the limit", body: "hello", limit: 10, want: "hello"},
		{name: "exactly at the limit", body: "hello", limit: 5, want: "hello"},
		{name: "one byte over", body: "hello!", limit: 5, wantErr: true},
		{name: "far over", body: strings.Repeat("x", 1000), limit: 8, wantErr: true},
		{name: "empty body", body: "", limit: 4, want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadAtMost(strings.NewReader(tc.body), tc.limit)
			if tc.wantErr {
				if !errors.Is(err, ErrTooLarge) {
					t.Fatalf("err = %v, want ErrTooLarge", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A segment is held whole in memory by every fetch worker, so an origin that
// serves an enormous body — by malice, misconfiguration, or by returning an
// error page where a segment was expected — must be refused rather than
// buffered.
func TestFetcherRejectsOversizedSegment(t *testing.T) {
	const limit = 1024

	t.Run("declared Content-Length over the limit is refused", func(t *testing.T) {
		var served bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served = true
			w.Header().Set("Content-Length", strconv.Itoa(limit*10))
			w.WriteHeader(http.StatusOK)
			w.Write(make([]byte, limit*10))
		}))
		defer srv.Close()

		f := NewFetcher(nil, nil, limit, 0)
		res := f.Fetch(t.Context(), &queue.SegmentTask{URL: srv.URL}, false)
		if res.Err == nil {
			t.Fatalf("Fetch succeeded with %d bytes, want an error", len(res.Data))
		}
		if !errors.Is(res.Err, ErrTooLarge) {
			t.Errorf("err = %v, want ErrTooLarge", res.Err)
		}
		if !served {
			t.Error("origin was never contacted; the test proves nothing")
		}
	})

	t.Run("body exceeding the limit without Content-Length is refused", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// No Content-Length: chunked transfer, size unknown up front.
			w.Header().Set("Transfer-Encoding", "chunked")
			w.WriteHeader(http.StatusOK)
			for i := 0; i < 20; i++ {
				w.Write(make([]byte, limit/2))
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
			}
		}))
		defer srv.Close()

		f := NewFetcher(nil, nil, limit, 0)
		res := f.Fetch(t.Context(), &queue.SegmentTask{URL: srv.URL}, false)
		if res.Err == nil {
			t.Fatalf("Fetch succeeded with %d bytes, want an error", len(res.Data))
		}
		if !errors.Is(res.Err, ErrTooLarge) {
			t.Errorf("err = %v, want ErrTooLarge", res.Err)
		}
	})

	t.Run("a segment inside the limit still succeeds", func(t *testing.T) {
		payload := make([]byte, limit-1)
		for i := range payload {
			payload[i] = byte(i)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(payload)
		}))
		defer srv.Close()

		f := NewFetcher(nil, nil, limit, 0)
		res := f.Fetch(t.Context(), &queue.SegmentTask{URL: srv.URL}, false)
		if res.Err != nil {
			t.Fatalf("unexpected error: %v", res.Err)
		}
		if len(res.Data) != len(payload) {
			t.Errorf("got %d bytes, want %d", len(res.Data), len(payload))
		}
	})
}

// A byte-range request states exactly how many bytes it wants, so the range
// itself is the bound — an origin ignoring Range and returning the whole file
// must not be buffered.
func TestFetcherRangeIsBoundedByTheRequestedRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ignore the Range header entirely and return a large full body.
		w.WriteHeader(http.StatusOK)
		w.Write(make([]byte, 1<<20))
	}))
	defer srv.Close()

	f := NewFetcher(nil, nil, 1<<30, 0) // segment ceiling far above the range
	res := f.Fetch(t.Context(), &queue.SegmentTask{
		URL: srv.URL, RangeStart: 0, RangeEnd: 99, // asks for 100 bytes
	}, false)

	if res.Err == nil {
		t.Fatalf("Fetch returned %d bytes for a 100-byte range, want an error", len(res.Data))
	}
	if !errors.Is(res.Err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", res.Err)
	}
}

func TestFetcherRangeWithinBoundsSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		w.Write(make([]byte, 100))
	}))
	defer srv.Close()

	f := NewFetcher(nil, nil, 1<<30, 0)
	res := f.Fetch(t.Context(), &queue.SegmentTask{
		URL: srv.URL, RangeStart: 0, RangeEnd: 99,
	}, false)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if len(res.Data) != 100 {
		t.Errorf("got %d bytes, want 100", len(res.Data))
	}
}
