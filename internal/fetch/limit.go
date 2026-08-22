package fetch

import (
	"errors"
	"io"
	"time"
)

// ErrTooLarge reports that an upstream response exceeded the size the caller was
// willing to hold in memory.
var ErrTooLarge = errors.New("upstream response too large")

const (
	// DefaultMaxSegmentBytes bounds a single media segment held in memory. Every
	// fetch worker buffers a whole segment, so the real exposure is this times
	// worker count times channel count — but the ceiling is a crash guard, not a
	// tuning knob: in normal operation the response's own Content-Length bounds
	// the read, and this only fires when an origin declares (or streams) a body
	// no real segment would reach. 128 MiB leaves room for ~10s of 80 Mbps 4K.
	DefaultMaxSegmentBytes int64 = 128 << 20

	// DefaultSegmentTimeout bounds one segment download end to end, including
	// reading the body — it is not an idle timeout, so a slow-but-progressing
	// transfer is cut off just the same. It therefore has to cover the largest
	// segment divided by the slowest usable bandwidth, which makes it genuinely
	// deployment-specific: 30s suits a few Mbps of segment bitrate, and a
	// 20 Mbps source over a narrow link needs considerably more. Configurable
	// as worker.segment_timeout.
	DefaultSegmentTimeout = 30 * time.Second

	// MaxManifestBytes bounds an upstream MPD or HLS playlist. Manifests are text
	// and run to a few MB at the very most, so this needs no per-deployment
	// tuning — it exists so a misbehaving origin cannot stream indefinitely into
	// the watcher goroutine.
	MaxManifestBytes int64 = 32 << 20
)

// ReadAtMost reads r fully, failing with ErrTooLarge if it yields more than
// limit bytes. A body of exactly limit bytes is accepted.
//
// Upstream bodies are controlled by whoever operates the origin — and, if that
// origin is compromised or simply broken, by an attacker — so every read of one
// is bounded. A limit of zero or less means unlimited, which callers should
// avoid.
func ReadAtMost(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(r)
	}
	// Read one byte past the limit so a body sitting exactly on it still
	// succeeds while anything beyond is detectable.
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrTooLarge
	}
	return data, nil
}
