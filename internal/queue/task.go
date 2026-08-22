// Package queue provides the priority task queue for segment download scheduling.
package queue

import (
	"strconv"
	"time"
)

// SegmentFormat identifies the container the segment bytes are in. It decides
// which structural validation runs and which extension the file is stored under.
//
// FormatFMP4 is the zero value on purpose: every existing DASH construction
// site keeps producing fMP4 tasks without naming the field.
type SegmentFormat uint8

const (
	FormatFMP4 SegmentFormat = iota
	FormatTS
	FormatWebVTT
)

// HLSFields are the parts of a task that only an HLS source populates. DASH
// leaves every one of them at its zero value.
//
// They are embedded in SegmentTask rather than split into a separate task type
// because the broker and the fetch pool are deliberately shared by both
// protocols: one queue, one worker pool, one Result. Grouping them answers
// "does this field apply to my protocol?" in the type, while Go's field
// promotion leaves every read and assignment site (task.KeyURI, task.StartPTS,
// …) reading exactly as it did before.
//
// Note the zero values fail open, not closed: an unpopulated HLSFields
// describes a cleartext segment. That is correct for DASH, which is why it is
// the default — but it means a construction site that forgets to fill these in
// for an encrypted HLS source produces a playlist with no EXT-X-KEY over
// segments that are still ciphertext, which plays as silence rather than
// failing loudly.
type HLSFields struct {
	// StartPTS is the presentation start time in Timescale units, supplied by
	// the source at discovery time. Only used for FormatTS; fMP4 reads the
	// authoritative value out of the segment's tfdt box instead. Carrying it on
	// the task keeps the processor stateless about ordering, so out-of-order
	// fetch completion cannot corrupt a running accumulator.
	StartPTS int64

	// Discontinuity marks a timeline break immediately before this segment
	// (HLS EXT-X-DISCONTINUITY).
	Discontinuity bool

	// PersistMeta asks the processor to record this segment's metadata in the
	// per-representation sidecar. HLS sets it (both TS and fMP4) because its
	// timing, discontinuity, and key material cannot be fully recovered from the
	// files alone; DASH leaves it false since fMP4 tfdt already carries timing.
	PersistMeta bool

	// Encrypted marks a segment protected by HLS AES-128 (EXT-X-KEY). KeyURI is
	// the upstream key URL and IV the 16-byte initialization vector, either
	// taken from the tag or derived from the media sequence number.
	Encrypted bool
	KeyURI    string
	IV        []byte

	// DecryptOnIngest asks the processor to decrypt before storing, so the
	// segment lands on disk as cleartext and the outbound playlist carries no
	// EXT-X-KEY. When false the ciphertext is stored and served untouched, and
	// players fetch the key through the gateway's key proxy. Which mode applies
	// is a per-channel setting resolved by the source, keeping the processor
	// free of channel policy.
	DecryptOnIngest bool
}

// SegmentTask describes a single segment that needs to be fetched.
type SegmentTask struct {
	ChannelID string
	PeriodID  string
	ASID      string // AdaptationSet ID
	RepID     string
	MediaType string // "video" / "audio" / "text"
	SegNo     uint64
	URL       string // resolved absolute URL
	InitURL   string // init segment URL (same for all segs of this rep)

	// Format selects the container. MPEG-TS segments have no moof/tfdt, so
	// their timing cannot be recovered from the bytes and must be supplied by
	// the source (see StartPTS).
	Format SegmentFormat

	// HLSFields groups the parts only an HLS source populates.
	HLSFields

	// Byte range for HTTP Range requests (SegmentBase / SegmentList byte-range).
	// Both zero means full-file download.
	RangeStart uint64
	RangeEnd   uint64

	// Byte range for the init segment (SegmentBase with <Initialization range="...">).
	// Both zero means full-file download of InitURL.
	InitRangeStart uint64
	InitRangeEnd   uint64

	// FallbackURLs holds alternative segment URLs for CDN retry.
	// Populated from <BaseURL>[1:] elements in the MPD. On each retry the
	// processor rotates: FallbackURLs[0] becomes URL and current URL is appended.
	FallbackURLs []string

	// Priority: lower value = higher urgency (closer to live edge gets 0).
	// For static VOD, all tasks start at the same priority and decay over retries.
	Priority int

	// Deadline after which this task is no longer worth fetching (live only).
	// Zero means no deadline (static VOD).
	Deadline time.Time

	// Segment duration and timescale (from SegmentTemplate/SegmentList), used to compute EndPTS.
	Duration  uint64 // in timescale units
	Timescale uint32

	// Retry state
	Retries   int
	NextRetry time.Time
}

// Expired returns true if the task has passed its usefulness deadline.
func (t *SegmentTask) Expired() bool {
	return !t.Deadline.IsZero() && time.Now().After(t.Deadline)
}

// Key returns a unique string identifying this task.
func (t *SegmentTask) Key() string {
	return RepKey(t.ChannelID, t.PeriodID, t.ASID, t.RepID) + "/" + strconv.FormatUint(t.SegNo, 10)
}

// RepKey identifies one representation within a channel. It is Key() without
// the segment number, and is what both ingest paths use to key their
// per-representation bookkeeping.
//
// It lives here, beside Key, because the two must agree: Key is defined as
// RepKey plus a segment number, and a divergence between them would break
// broker deduplication in a way no test asserts directly.
func RepKey(channelID, periodID, asID, repID string) string {
	return channelID + "/" + periodID + "/" + asID + "/" + repID
}

// RepKey returns the rep-level key for this task.
func (t *SegmentTask) RepKey() string {
	return RepKey(t.ChannelID, t.PeriodID, t.ASID, t.RepID)
}
