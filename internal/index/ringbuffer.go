package index

// RingBuffer is a fixed-capacity circular buffer of SegmentState values.
// When full, the oldest entry is overwritten.
// capacity=0 creates an unbounded (append-only) buffer used for VOD rebuilds.
// Not safe for concurrent use — callers must hold the outer lock.
type RingBuffer struct {
	buf  []SegmentState
	head int // index of next write (unused in unbounded mode)
	size int // number of valid entries
	cap  int // 0 = unbounded
	// pos maps a segment number to its slot in buf. The same segment can be
	// fetched twice — the HLS backfill path re-enqueues a segment that is still
	// in flight, because an uncommitted segment is indistinguishable from a hole
	// — and two entries for one number look like a numbering break to the
	// playlist generator, which then truncates the window. Keeping this index
	// makes Push replace rather than duplicate.
	pos map[uint64]int
}

// NewRingBuffer creates a ring buffer with the given capacity.
// Pass capacity=0 for an unbounded buffer (VOD mode).
func NewRingBuffer(capacity int) *RingBuffer {
	if capacity < 0 {
		capacity = 64
	}
	if capacity == 0 {
		return &RingBuffer{cap: 0, pos: make(map[uint64]int)}
	}
	return &RingBuffer{
		buf: make([]SegmentState, capacity),
		cap: capacity,
		pos: make(map[uint64]int, capacity),
	}
}

// Push stores a segment, replacing any existing entry with the same segment
// number. If the buffer is fixed-capacity and full, the oldest entry is
// silently overwritten. In unbounded mode the slice grows without limit.
//
// It reports whether the segment was new. A false result means an existing entry
// was replaced, which callers counting throughput must not count again — the HLS
// backfill path genuinely re-fetches segments that are still in flight (see the
// pos field's comment), so treating every Push as progress would let a download
// bar climb past its own total.
func (r *RingBuffer) Push(s SegmentState) bool {
	if r.pos == nil {
		r.pos = make(map[uint64]int)
	}
	if at, ok := r.pos[s.SegNo]; ok {
		r.buf[at] = s // same segment fetched again: keep the later state
		return false
	}

	if r.cap == 0 {
		// Unbounded: simple append.
		r.buf = append(r.buf, s)
		r.pos[s.SegNo] = len(r.buf) - 1
		r.size++
		return true
	}

	if r.size == r.cap {
		// This slot is about to be reused; drop the evicted segment's index
		// entry, but only if it still owns the slot.
		evicted := r.buf[r.head].SegNo
		if at, ok := r.pos[evicted]; ok && at == r.head {
			delete(r.pos, evicted)
		}
	}
	r.buf[r.head] = s
	r.pos[s.SegNo] = r.head
	r.head = (r.head + 1) % r.cap
	if r.size < r.cap {
		r.size++
	}
	return true
}

// Snapshot returns a slice of all valid entries in insertion order (oldest first).
func (r *RingBuffer) Snapshot() []SegmentState {
	if r.size == 0 {
		return nil
	}
	if r.cap == 0 {
		// Unbounded: buf slice is already in order.
		out := make([]SegmentState, r.size)
		copy(out, r.buf)
		return out
	}
	out := make([]SegmentState, r.size)
	if r.size < r.cap {
		copy(out, r.buf[:r.size])
	} else {
		// full: oldest is at r.head
		n := copy(out, r.buf[r.head:])
		copy(out[n:], r.buf[:r.head])
	}
	return out
}

// UpdateWhere calls fn on every entry in the buffer in place.
func (r *RingBuffer) UpdateWhere(fn func(*SegmentState)) {
	for i := range r.buf {
		fn(&r.buf[i])
	}
}

// Len returns the number of valid entries.
func (r *RingBuffer) Len() int { return r.size }
