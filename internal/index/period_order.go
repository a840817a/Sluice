package index

import (
	"slices"
	"strconv"
	"strings"
)

// OrderPeriodIDs returns ids in timeline order.
//
// A DASH source names its periods freely, so their order is the order the
// origin's MPD lists them in: pass that as docOrder. A period the gateway
// still holds but docOrder no longer lists has rolled off the origin's MPD,
// so it is older than every period listed and comes first. A period started
// by an origin restart (RestartPeriodID) follows the period it restarted, and
// later restarts follow earlier ones.
//
// An HLS source names its periods by period number, a decimal number the
// gateway increments on every origin reset, so numbers sort numerically — as
// strings, period 10 would come before period 2. With no docOrder to go by,
// numbers come before other names.
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
		baseA, genA := splitRestartPeriodID(a)
		baseB, genB := splitRestartPeriodID(b)
		pa, aListed := pos[baseA]
		pb, bListed := pos[baseB]
		switch {
		case aListed && bListed:
			if pa != pb {
				return pa - pb
			}
		case aListed:
			return 1
		case bListed:
			return -1
		default:
			if c := comparePeriodIDs(baseA, baseB); c != 0 {
				return c
			}
		}
		return genA - genB
	})
	return out
}

// restartMarker joins an origin's period ID to the number of the restart that
// started a gateway period under it.
const restartMarker = ".r"

// RestartPeriodID names the period the gateway starts when the origin restarts
// its numbering inside its period originID for the gen-th time (ADR-0007).
// Generation 0 is originID itself.
func RestartPeriodID(originID string, gen int) string {
	if gen <= 0 {
		return originID
	}
	return originID + restartMarker + strconv.Itoa(gen)
}

// splitRestartPeriodID returns the origin period ID and restart generation a
// period ID stands for; an ID RestartPeriodID did not produce is generation 0
// of itself.
func splitRestartPeriodID(id string) (originID string, gen int) {
	i := strings.LastIndex(id, restartMarker)
	if i < 0 {
		return id, 0
	}
	n, err := strconv.Atoi(id[i+len(restartMarker):])
	if err != nil || n <= 0 || strconv.Itoa(n) != id[i+len(restartMarker):] {
		return id, 0
	}
	return id[:i], n
}

// RestartGeneration returns the restart generation of id as a period of
// originID: 0 for originID itself, n for RestartPeriodID(originID, n), and
// false for a period of some other origin period.
func RestartGeneration(id, originID string) (int, bool) {
	base, gen := splitRestartPeriodID(id)
	return gen, base == originID
}

// OriginPeriodID returns the origin's Period ID behind gateway period id: id
// itself, unless RestartPeriodID started it after an origin restart.
func OriginPeriodID(id string) string {
	base, _ := splitRestartPeriodID(id)
	return base
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
