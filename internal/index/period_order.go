package index

import (
	"slices"
	"strconv"
)

// OrderPeriodIDs returns ids in timeline order.
//
// A DASH source names its periods freely, so their order is the order the
// origin's MPD lists them in: pass that as docOrder. An HLS source names them
// by period number, a decimal number the gateway increments on every origin
// reset, so numbers sort numerically — as strings, period 10 would come
// before period 2. Periods docOrder does not mention follow those it does,
// numbers before other names.
//
// It is the one place period order is decided. Anything that lays periods out
// in time — playlists, synthesized manifests, the snapshot — must use it, or
// two outputs of the same channel disagree about which stream came first.
func OrderPeriodIDs(ids []string, docOrder []string) []string {
	pos := make(map[string]int, len(docOrder))
	for i, id := range docOrder {
		if _, dup := pos[id]; !dup {
			pos[id] = i
		}
	}
	out := slices.Clone(ids)
	slices.SortStableFunc(out, func(a, b string) int {
		pa, aKnown := pos[a]
		pb, bKnown := pos[b]
		switch {
		case aKnown && bKnown:
			return pa - pb
		case aKnown:
			return -1
		case bKnown:
			return 1
		}
		return comparePeriodIDs(a, b)
	})
	return out
}

// comparePeriodIDs orders two period IDs with no document to go by: decimal
// numbers numerically and before anything else, the rest as strings.
func comparePeriodIDs(a, b string) int {
	na, aErr := strconv.ParseUint(a, 10, 64)
	nb, bErr := strconv.ParseUint(b, 10, 64)
	switch {
	case aErr == nil && bErr == nil:
		switch {
		case na < nb:
			return -1
		case na > nb:
			return 1
		}
		return 0
	case aErr == nil:
		return -1
	case bErr == nil:
		return 1
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
