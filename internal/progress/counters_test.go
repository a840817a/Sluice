package progress

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNilReceiverIsANoOp pins that every mutator tolerates a nil Counters, which
// is what lets callers treat progress tracking as optional. If this breaks, every
// Processor and Watcher constructed without counters panics on its first segment.
func TestNilReceiverIsANoOp(t *testing.T) {
	var c *Counters
	c.AddStored(1, 100)
	c.SeedStored(5)
	c.AddFailed(1)
	c.RecordSegmentError("fetch", "boom", "v0", 7)
	c.SetSourceError("poll", "boom")
	c.ClearSourceError()
	c.SetExpectedTotal(10)
	c.MarkCapped()
	c.Finalize()

	got := c.Snapshot()
	if got != (Snapshot{}) {
		t.Errorf("nil Snapshot() = %+v, want zero value", got)
	}
	if d, ok := got.ETA(); ok || d != 0 {
		t.Errorf("zero Snapshot ETA() = (%v, %v), want (0, false)", d, ok)
	}
}

func TestAddStoredAccumulates(t *testing.T) {
	c := New()
	c.AddStored(1, 1000)
	c.AddStored(2, 2500)
	c.AddStored(0, 999) // zero must not move anything

	s := c.Snapshot()
	if s.SegmentsStored != 3 {
		t.Errorf("SegmentsStored = %d, want 3", s.SegmentsStored)
	}
	if s.BytesStored != 3500 {
		t.Errorf("BytesStored = %d, want 3500", s.BytesStored)
	}
	if s.LastSegmentAt.IsZero() {
		t.Error("LastSegmentAt is zero after storing")
	}
	if s.StartedAt.IsZero() {
		t.Error("StartedAt is zero")
	}
}

func TestSnapshotBeforeAnyStore(t *testing.T) {
	s := New().Snapshot()
	if !s.LastSegmentAt.IsZero() {
		t.Errorf("LastSegmentAt = %v, want zero before any store", s.LastSegmentAt)
	}
	if s.TotalKnown {
		t.Error("TotalKnown is true before discovery ran")
	}
	if s.SegmentsPerSec != 0 {
		t.Errorf("SegmentsPerSec = %v, want 0", s.SegmentsPerSec)
	}
}

// TestSeedStoredDoesNotCreateThroughput covers the restart path: an index rebuilt
// from disk restores thousands of segments that were not fetched during this run.
// Counting them as throughput would render an absurd rate and a nonsense ETA.
func TestSeedStoredDoesNotCreateThroughput(t *testing.T) {
	c := New()
	c.SeedStored(5000)

	s := c.Snapshot()
	if s.SegmentsStored != 5000 {
		t.Errorf("SegmentsStored = %d, want 5000", s.SegmentsStored)
	}
	if s.SegmentsPerSec != 0 {
		t.Errorf("SegmentsPerSec = %v, want 0 after seeding", s.SegmentsPerSec)
	}
	if !s.LastSegmentAt.IsZero() {
		t.Error("LastSegmentAt should stay zero after seeding")
	}
}

// TestRateIsWindowedNotLifetimeAverage is the reason this package computes rates
// the way it does. A channel that ingested fast and then stalled must report ~0,
// not the healthy average it had before the stall.
func TestRateIsWindowedNotLifetimeAverage(t *testing.T) {
	now := time.Now()
	c := New()
	c.stored.Store(1000)
	c.bytes.Store(1 << 20)
	// One sample, well outside the window: the channel has not stored anything
	// for 40 seconds.
	c.samples = []sample{{at: now.Add(-40 * time.Second), stored: 0, bytes: 0}}

	seg, byt := c.rates(now, 1000, 1<<20)
	if seg != 0 || byt != 0 {
		t.Errorf("stalled rates = (%v, %v), want (0, 0)", seg, byt)
	}
	if len(c.samples) != 0 {
		t.Errorf("expired samples not trimmed: %d remain", len(c.samples))
	}
}

func TestRateOverLiveWindow(t *testing.T) {
	now := time.Now()
	c := New()
	c.samples = []sample{{at: now.Add(-10 * time.Second), stored: 0, bytes: 0}}

	seg, byt := c.rates(now, 100, 5000)
	if seg < 9.9 || seg > 10.1 {
		t.Errorf("SegmentsPerSec = %v, want ~10", seg)
	}
	if byt < 499 || byt > 501 {
		t.Errorf("BytesPerSec = %v, want ~500", byt)
	}
}

// TestSampleBaselineIsPreIncrement guards the off-by-one that would undercount a
// window by one segment: the sample must record the counts as they were *before*
// the store that created it.
func TestSampleBaselineIsPreIncrement(t *testing.T) {
	c := New()
	c.AddStored(1, 100)

	c.rateMu.Lock()
	n := len(c.samples)
	var first sample
	if n > 0 {
		first = c.samples[0]
	}
	c.rateMu.Unlock()

	if n != 1 {
		t.Fatalf("len(samples) = %d, want 1", n)
	}
	if first.stored != 0 || first.bytes != 0 {
		t.Errorf("first sample = {stored:%d bytes:%d}, want pre-increment zeros", first.stored, first.bytes)
	}
}

func TestSamplesAreThrottled(t *testing.T) {
	c := New()
	for i := 0; i < 500; i++ {
		c.AddStored(1, 10)
	}
	c.rateMu.Lock()
	n := len(c.samples)
	c.rateMu.Unlock()

	// 500 immediate stores fall inside one minSampleInterval, so only the first
	// records a sample. Without throttling this slice would hold 500 entries.
	if n > 3 {
		t.Errorf("len(samples) = %d after 500 rapid stores, want <= 3", n)
	}
	if got := c.Snapshot().SegmentsStored; got != 500 {
		t.Errorf("SegmentsStored = %d, want 500 — throttling must not drop counts", got)
	}
}

func TestSetExpectedTotalLargestWins(t *testing.T) {
	c := New()
	c.SetExpectedTotal(0) // ignored: 0 means unknown, not "zero segments"
	if c.Snapshot().TotalKnown {
		t.Error("TotalKnown set by SetExpectedTotal(0)")
	}

	c.SetExpectedTotal(100)
	c.SetExpectedTotal(40) // a later, smaller per-variant count must not shrink it
	c.SetExpectedTotal(250)

	s := c.Snapshot()
	if s.ExpectedSegments != 250 {
		t.Errorf("ExpectedSegments = %d, want 250", s.ExpectedSegments)
	}
	if !s.TotalKnown {
		t.Error("TotalKnown = false after SetExpectedTotal")
	}
}

func TestExpectedTotalSurvivesFinalize(t *testing.T) {
	// The point of keeping expected and stored separate: a finished channel that
	// lost 12 segments must still be able to say 500/512, not 500/500.
	c := New()
	c.SetExpectedTotal(512)
	c.AddStored(500, 0)
	c.AddFailed(12)
	c.Finalize()

	s := c.Snapshot()
	if s.ExpectedSegments != 512 || s.SegmentsStored != 500 || s.SegmentsFailed != 12 {
		t.Errorf("got stored=%d expected=%d failed=%d, want 500/512/12",
			s.SegmentsStored, s.ExpectedSegments, s.SegmentsFailed)
	}
	if !s.Finalized {
		t.Error("Finalized = false")
	}
}

func TestETAConditions(t *testing.T) {
	tests := []struct {
		name string
		snap Snapshot
		want bool
	}{
		{"healthy", Snapshot{TotalKnown: true, ExpectedSegments: 100, SegmentsStored: 50, SegmentsPerSec: 5}, true},
		{"no total (live)", Snapshot{ExpectedSegments: 0, SegmentsStored: 50, SegmentsPerSec: 5}, false},
		{"capped total is a floor", Snapshot{TotalKnown: true, TotalCapped: true, ExpectedSegments: 100, SegmentsStored: 50, SegmentsPerSec: 5}, false},
		{"finalized", Snapshot{TotalKnown: true, Finalized: true, ExpectedSegments: 100, SegmentsStored: 50, SegmentsPerSec: 5}, false},
		{"stalled", Snapshot{TotalKnown: true, ExpectedSegments: 100, SegmentsStored: 50, SegmentsPerSec: 0}, false},
		{"already past total", Snapshot{TotalKnown: true, ExpectedSegments: 100, SegmentsStored: 100, SegmentsPerSec: 5}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := tc.snap.ETA()
			if ok != tc.want {
				t.Fatalf("ETA() ok = %v, want %v", ok, tc.want)
			}
			if ok && d != 10*time.Second {
				t.Errorf("ETA() = %v, want 10s", d)
			}
		})
	}
}

func TestSegmentErrorIsStickyAndCounted(t *testing.T) {
	c := New()
	c.RecordSegmentError("fetch", "connection reset", "video-0", 42)

	s := c.Snapshot()
	if s.SegmentsFailed != 1 {
		t.Errorf("SegmentsFailed = %d, want 1", s.SegmentsFailed)
	}
	if s.LastSegmentError == nil {
		t.Fatal("LastSegmentError is nil")
	}
	if s.LastSegmentError.Stage != "fetch" || s.LastSegmentError.RepID != "video-0" || s.LastSegmentError.SegNo != 42 {
		t.Errorf("LastSegmentError = %+v", s.LastSegmentError)
	}

	// A later failure replaces it; nothing clears it.
	c.RecordSegmentError("write", "disk full", "audio-0", 43)
	s = c.Snapshot()
	if s.SegmentsFailed != 2 {
		t.Errorf("SegmentsFailed = %d, want 2", s.SegmentsFailed)
	}
	if s.LastSegmentError.Stage != "write" {
		t.Errorf("LastSegmentError.Stage = %q, want write", s.LastSegmentError.Stage)
	}
}

func TestSourceErrorClears(t *testing.T) {
	c := New()
	if c.Snapshot().SourceError != nil {
		t.Fatal("SourceError set on a fresh Counters")
	}
	c.SetSourceError("poll", "502 Bad Gateway")
	if got := c.Snapshot().SourceError; got == nil || got.Stage != "poll" {
		t.Fatalf("SourceError = %+v, want stage poll", got)
	}
	// Unlike a segment error, an upstream error is a live condition.
	c.ClearSourceError()
	if got := c.Snapshot().SourceError; got != nil {
		t.Errorf("SourceError = %+v after clear, want nil", got)
	}
	// A source error must not inflate the segment failure count.
	if got := c.Snapshot().SegmentsFailed; got != 0 {
		t.Errorf("SegmentsFailed = %d, want 0", got)
	}
}

func TestSanitizeMessage(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"flattens newlines", "line one\nline two\r\n\tline three", "line one line two line three"},
		{"collapses runs of spaces", "a     b", "a b"},
		{"trims", "  padded  ", "padded"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeMessage(tc.in); got != tc.want {
				t.Errorf("sanitizeMessage(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	long := sanitizeMessage(strings.Repeat("x", 500))
	if n := len([]rune(long)); n != maxMessageRunes+1 { // +1 for the ellipsis
		t.Errorf("truncated length = %d runes, want %d", n, maxMessageRunes+1)
	}
	if !strings.HasSuffix(long, "…") {
		t.Error("truncated message lacks an ellipsis")
	}

	// Multi-byte input must be cut on a rune boundary, not mid-sequence.
	cjk := sanitizeMessage(strings.Repeat("串", 500))
	if !strings.HasPrefix(cjk, "串串") {
		t.Errorf("multi-byte truncation corrupted the string: %q", cjk[:12])
	}
	if n := len([]rune(cjk)); n != maxMessageRunes+1 {
		t.Errorf("multi-byte truncated length = %d runes, want %d", n, maxMessageRunes+1)
	}
}

// TestConcurrentUse is the real guard on this type: it is written by ingest
// goroutines while HTTP handlers snapshot it. Run with -race.
func TestConcurrentUse(t *testing.T) {
	c := New()
	const writers, iterations = 100, 200

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				c.AddStored(1, 10)
				if j%20 == 0 {
					c.RecordSegmentError("fetch", "err", "rep", uint64(j))
					c.SetExpectedTotal(uint64(n * j))
					c.SetSourceError("poll", "flap")
					c.ClearSourceError()
				}
			}
		}(i)
	}
	// Readers race the writers.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				s := c.Snapshot()
				_, _ = s.ETA()
			}
		}()
	}
	wg.Wait()

	if got := c.Snapshot().SegmentsStored; got != writers*iterations {
		t.Errorf("SegmentsStored = %d, want %d", got, writers*iterations)
	}
}
