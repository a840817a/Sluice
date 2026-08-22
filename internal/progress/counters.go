// Package progress tracks per-channel ingest progress: how many segments have
// actually been stored, how many were expected, throughput, and the most recent
// failures.
//
// It exists as a leaf package — depending on nothing inside this module — because
// both the ingest layer and the channel layer need to touch the same counters,
// and internal/channel already imports the ingest packages. A counter type living in
// either of those would be an import cycle.
//
// Every method is safe on a nil receiver, so a Processor or Watcher that was
// never given a Counters simply records nothing. That keeps the counters
// entirely optional for callers (notably tests) that do not care about them.
package progress

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// rateWindow is how far back throughput samples reach. A sliding window is
	// used rather than stored/(now-startedAt) because the lifetime average keeps
	// reporting healthy throughput long after ingest has stalled — the one moment
	// an operator actually needs the number to move.
	rateWindow = 30 * time.Second

	// minSampleInterval throttles how often a throughput sample is recorded. It
	// bounds the sample slice to roughly rateWindow/minSampleInterval entries no
	// matter how fast segments arrive.
	minSampleInterval = 200 * time.Millisecond

	// maxMessageRunes bounds a stored error message. Messages originate from
	// upstream URLs and HTTP response bodies and are rendered into the admin UI,
	// so they are truncated here rather than at the display layer.
	maxMessageRunes = 200
)

// ErrorRecord is a single recorded failure. RepID and SegNo are zero for errors
// that are not attributable to one segment (an upstream manifest poll, say).
type ErrorRecord struct {
	At      time.Time `json:"at"`
	Stage   string    `json:"stage"`
	Message string    `json:"message"`
	RepID   string    `json:"rep_id,omitempty"`
	SegNo   uint64    `json:"seg_no,omitempty"`
}

// sample is one throughput observation: the cumulative counts as they stood
// immediately *before* the store recorded at time at. Holding the pre-increment
// value is what lets a window starting at this sample include the segment that
// created it, instead of undercounting by one.
type sample struct {
	at     time.Time
	stored uint64
	bytes  uint64
}

// Counters holds one channel's ingest progress. Create with New; the zero value
// is not usable because it has no start time.
type Counters struct {
	stored atomic.Uint64
	failed atomic.Uint64
	bytes  atomic.Uint64

	// expected is what discovery said the channel contains, and stays put once
	// set. It is deliberately not overwritten when ingest completes: keeping
	// "what we expected" separate from "what we stored" is what lets the UI say
	// 500/512 with 12 failures rather than silently rewriting history as 500/500.
	expected    atomic.Uint64
	expectedSet atomic.Bool
	capped      atomic.Bool
	finalized   atomic.Bool

	startedAt    atomic.Int64 // unix nanos
	lastStoredAt atomic.Int64 // unix nanos; 0 = nothing stored yet

	lastSegErr atomic.Pointer[ErrorRecord]
	sourceErr  atomic.Pointer[ErrorRecord]

	rateMu  sync.Mutex
	samples []sample
}

// New returns Counters whose clock starts now.
func New() *Counters {
	c := &Counters{}
	c.startedAt.Store(time.Now().UnixNano())
	return c
}

// AddStored records that n segments totalling nbytes were written to disk and
// committed to the index. Callers must only count segments that were genuinely
// new — see RepresentationState.Commit, which reports that.
func (c *Counters) AddStored(n, nbytes uint64) {
	if c == nil || n == 0 {
		return
	}
	now := time.Now()
	// Sample before applying the delta so the recorded baseline excludes it.
	c.recordSample(now, c.stored.Load(), c.bytes.Load())
	c.stored.Add(n)
	c.bytes.Add(nbytes)
	c.lastStoredAt.Store(now.UnixNano())
}

// SeedStored sets the stored count directly, for a channel whose index was
// rebuilt from disk at startup. It does not touch throughput samples: those
// segments were not fetched during this run and would otherwise show up as an
// enormous instantaneous rate.
func (c *Counters) SeedStored(n uint64) {
	if c == nil {
		return
	}
	c.stored.Store(n)
}

// AddFailed records n failed segment attempts without attaching a message.
//
// This counts *attempts*, not lost segments: most failures are requeued and
// succeed later, so a segment that needed three tries adds three here and still
// arrives. Segments genuinely abandoned are counted by Broker.Dropped instead.
// Conflating the two would make a "12 missing" claim in the UI a fabrication.
func (c *Counters) AddFailed(n uint64) {
	if c == nil || n == 0 {
		return
	}
	c.failed.Add(n)
}

// RecordSegmentError counts one failed attempt (see AddFailed for why that is not
// the same as a lost segment) and remembers it as the most recent failure. The
// record is sticky: it survives until another failure replaces it, so a UI
// polling every couple of seconds cannot miss it.
func (c *Counters) RecordSegmentError(stage, message, repID string, segNo uint64) {
	if c == nil {
		return
	}
	c.failed.Add(1)
	c.lastSegErr.Store(&ErrorRecord{
		At:      time.Now(),
		Stage:   stage,
		Message: sanitizeMessage(message),
		RepID:   repID,
		SegNo:   segNo,
	})
}

// SetSourceError records that the upstream itself is failing (a manifest or
// playlist poll). Unlike a segment error this is a live condition, so
// ClearSourceError removes it as soon as a poll succeeds.
func (c *Counters) SetSourceError(stage, message string) {
	if c == nil {
		return
	}
	c.sourceErr.Store(&ErrorRecord{
		At:      time.Now(),
		Stage:   stage,
		Message: sanitizeMessage(message),
	})
}

// ClearSourceError marks the upstream healthy again.
func (c *Counters) ClearSourceError() {
	if c == nil {
		return
	}
	c.sourceErr.Store(nil)
}

// SetExpectedTotal records how many segments the source is expected to yield.
// Only meaningful for a bounded (static / VOD) source; a live stream has no
// total and must leave this unset so the UI shows a cumulative count rather than
// inventing a denominator.
//
// The largest value wins. HLS discovers its total one variant at a time, and a
// DASH channel can add periods, so a later, larger count is an extension of the
// same source rather than a correction.
func (c *Counters) SetExpectedTotal(n uint64) {
	if c == nil || n == 0 {
		return
	}
	for {
		cur := c.expected.Load()
		if n <= cur {
			break
		}
		if c.expected.CompareAndSwap(cur, n) {
			break
		}
	}
	c.expectedSet.Store(true)
}

// MarkCapped records that discovery truncated its segment enumeration, making
// the expected total a floor rather than an exact figure. An ETA derived from a
// floor is a lie, so the UI must suppress it when this is set.
func (c *Counters) MarkCapped() {
	if c == nil {
		return
	}
	c.capped.Store(true)
}

// Finalize marks ingest complete: no further segments are coming. This is what
// distinguishes "downloaded 500 of 512, still working" from "finished, 12 never
// arrived".
func (c *Counters) Finalize() {
	if c == nil {
		return
	}
	c.finalized.Store(true)
}

// Snapshot is a consistent read of the counters at one instant.
type Snapshot struct {
	SegmentsStored uint64
	SegmentsFailed uint64
	BytesStored    uint64

	ExpectedSegments uint64
	TotalKnown       bool
	TotalCapped      bool
	Finalized        bool

	StartedAt     time.Time
	LastSegmentAt time.Time // zero if nothing has been stored

	SegmentsPerSec float64
	BytesPerSec    float64

	LastSegmentError *ErrorRecord
	SourceError      *ErrorRecord
}

// Snapshot reads every counter once and returns them together, so a caller
// rendering several fields cannot mix values from two different instants.
func (c *Counters) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	now := time.Now()
	stored := c.stored.Load()
	bytes := c.bytes.Load()
	segRate, byteRate := c.rates(now, stored, bytes)

	s := Snapshot{
		SegmentsStored:   stored,
		SegmentsFailed:   c.failed.Load(),
		BytesStored:      bytes,
		ExpectedSegments: c.expected.Load(),
		TotalKnown:       c.expectedSet.Load(),
		TotalCapped:      c.capped.Load(),
		Finalized:        c.finalized.Load(),
		SegmentsPerSec:   segRate,
		BytesPerSec:      byteRate,
		LastSegmentError: c.lastSegErr.Load(),
		SourceError:      c.sourceErr.Load(),
	}
	if ns := c.startedAt.Load(); ns != 0 {
		s.StartedAt = time.Unix(0, ns)
	}
	if ns := c.lastStoredAt.Load(); ns != 0 {
		s.LastSegmentAt = time.Unix(0, ns)
	}
	return s
}

// ETA returns how long the remaining segments are expected to take, and whether
// that estimate is meaningful at all. It is not meaningful without an exact
// total (a capped total is a floor), without forward progress, or once ingest
// has finished.
func (s Snapshot) ETA() (time.Duration, bool) {
	if !s.TotalKnown || s.TotalCapped || s.Finalized {
		return 0, false
	}
	if s.SegmentsPerSec <= 0 || s.SegmentsStored >= s.ExpectedSegments {
		return 0, false
	}
	remaining := float64(s.ExpectedSegments - s.SegmentsStored)
	return time.Duration(remaining / s.SegmentsPerSec * float64(time.Second)), true
}

// recordSample appends a throughput observation, throttled to minSampleInterval
// and trimmed to rateWindow.
func (c *Counters) recordSample(now time.Time, stored, bytes uint64) {
	c.rateMu.Lock()
	defer c.rateMu.Unlock()
	if n := len(c.samples); n > 0 && now.Sub(c.samples[n-1].at) < minSampleInterval {
		return
	}
	c.samples = append(c.samples, sample{at: now, stored: stored, bytes: bytes})
	c.trimLocked(now)
}

// rates computes throughput over the window ending at now. Anchoring the right
// edge to now rather than to the newest sample is what makes a stalled channel
// decay to zero instead of reporting its last healthy average forever.
func (c *Counters) rates(now time.Time, stored, bytes uint64) (segPerSec, bytesPerSec float64) {
	c.rateMu.Lock()
	defer c.rateMu.Unlock()
	c.trimLocked(now)
	if len(c.samples) == 0 {
		return 0, 0
	}
	oldest := c.samples[0]
	dt := now.Sub(oldest.at).Seconds()
	if dt <= 0 {
		return 0, 0
	}
	if stored > oldest.stored {
		segPerSec = float64(stored-oldest.stored) / dt
	}
	if bytes > oldest.bytes {
		bytesPerSec = float64(bytes-oldest.bytes) / dt
	}
	return segPerSec, bytesPerSec
}

// trimLocked drops samples that have fallen out of the window. Callers hold rateMu.
func (c *Counters) trimLocked(now time.Time) {
	cutoff := now.Add(-rateWindow)
	i := 0
	for i < len(c.samples) && c.samples[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		c.samples = append(c.samples[:0], c.samples[i:]...)
	}
}

// sanitizeMessage flattens an error string into something safe to put in a JSON
// field and render in a card: single line, bounded length.
func sanitizeMessage(msg string) string {
	msg = strings.Join(strings.Fields(msg), " ")
	runes := []rune(msg)
	if len(runes) > maxMessageRunes {
		return string(runes[:maxMessageRunes]) + "…"
	}
	return msg
}
