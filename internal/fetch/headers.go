package fetch

import (
	"net/http"
	"sync/atomic"
)

// Headers supplies the per-channel upstream headers applied to every outbound
// request. It exists so a channel's credentials can be rotated while ingest is
// running: the admin API swaps the whole set rather than restarting the channel,
// which would reset the DVR window and the HLS media sequence.
//
// INVARIANT: the map returned by Snapshot is never modified in place, by anyone.
// It is read concurrently by every fetch worker and by both watcher poll loops
// with no synchronisation beyond the atomic load, so a single in-place write
// anywhere is a data race across all of them. Rotation replaces the map.
type Headers interface {
	// Snapshot returns the current header set. The result may be nil, and must
	// be treated as read-only.
	Snapshot() map[string]string
}

// AtomicHeaders is the Headers implementation used in production. The zero
// value is usable and reports no headers.
type AtomicHeaders struct {
	v atomic.Pointer[map[string]string]
}

// NewAtomicHeaders creates a provider holding a copy of h. Passing nil is
// valid and means "no extra headers".
func NewAtomicHeaders(h map[string]string) *AtomicHeaders {
	a := &AtomicHeaders{}
	a.Store(h)
	return a
}

// Store replaces the header set. It copies h so a caller that keeps mutating
// its own map cannot violate the read-only invariant after the fact.
func (a *AtomicHeaders) Store(h map[string]string) {
	if a == nil {
		return
	}
	cp := make(map[string]string, len(h))
	for k, v := range h {
		cp[k] = v
	}
	a.v.Store(&cp)
}

// Snapshot returns the current header set as a read-only map. A nil receiver
// returns nil, so call sites that have no headers configured can hold a nil
// *AtomicHeaders instead of branching.
func (a *AtomicHeaders) Snapshot() map[string]string {
	if a == nil {
		return nil
	}
	if m := a.v.Load(); m != nil {
		return *m
	}
	return nil
}

// ApplyHeaders sets the current header set on req. Every upstream request in
// this codebase goes through it, so rotation reaches segment fetches, manifest
// and playlist polls, and AES key requests alike.
//
// h may be nil — channels with no upstream auth, and most tests, pass nil — and
// a nil interface cannot be called, so the check belongs here rather than at
// each of the call sites.
func ApplyHeaders(req *http.Request, h Headers) {
	if h == nil {
		return
	}
	for k, v := range h.Snapshot() {
		req.Header.Set(k, v)
	}
}
