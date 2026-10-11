package channel

import (
	"errors"
	"fmt"
	"os"

	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/store"
)

// vodOriginMPD returns the origin MPD a VOD channel's DASH manifests are
// generated from: final, the last MPD the origin served, with every period
// the VOD index holds that final no longer lists put back in timeline order
// (ADR-0008).
//
// final lists only what the origin still offered at the end. A period that
// rolled off it, or the gateway period an origin restart ended (ADR-0007), is
// still on disk and in the VOD index, and without a description of its
// AdaptationSets and representations no DASH manifest can name its segments.
// Each such period is described by the origin MPD saved in its directory
// while it was listed. A period stored before those copies existed borrows
// the description of the period in final that has the same origin Period ID,
// which holds for a restarted stream; one with neither is left out and
// reported as a warning, as ADR-0004 has the VOD complete regardless.
//
// final itself is not modified. A nil final yields nil.
func vodOriginMPD(final *imdp.ParsedMPD, snap index.Snapshot, dataDir, channelID string) (*imdp.ParsedMPD, []string) {
	if final == nil {
		return nil, nil
	}
	byID := make(map[string]*imdp.ParsedPeriod, len(final.Periods))
	docOrder := make([]string, 0, len(final.Periods))
	for _, p := range final.Periods {
		byID[p.ID] = p
		docOrder = append(docOrder, p.ID)
	}
	var warnings []string
	for _, id := range snap.PeriodIDs(docOrder) {
		if _, listed := byID[id]; listed {
			continue
		}
		p, err := savedOriginPeriod(dataDir, channelID, id)
		if p == nil {
			p = renamedPeriod(sameOriginPeriod(final, id), id)
		}
		if p == nil {
			if err == nil {
				err = errors.New("no saved origin MPD")
			}
			warnings = append(warnings, fmt.Sprintf("period %s left out of the DASH manifests: %v", id, err))
			continue
		}
		byID[id] = p
	}
	if len(byID) == len(final.Periods) {
		return final, warnings
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	out := *final
	out.Periods = make([]*imdp.ParsedPeriod, 0, len(ids))
	for _, id := range index.OrderPeriodIDs(ids, docOrder) {
		out.Periods = append(out.Periods, byID[id])
	}
	return &out, warnings
}

// savedOriginPeriod reads gateway period id from the origin MPD saved in its
// directory. A missing copy is not an error.
func savedOriginPeriod(dataDir, channelID, id string) (*imdp.ParsedPeriod, error) {
	raw, err := os.ReadFile(store.PeriodOriginMPDPath(dataDir, channelID, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	saved, err := imdp.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("saved origin MPD: %w", err)
	}
	origin := index.OriginPeriodID(id)
	for _, p := range saved.Periods {
		if p.ID == origin {
			return renamedPeriod(p, id), nil
		}
	}
	return nil, fmt.Errorf("saved origin MPD does not list period %s", origin)
}

// sameOriginPeriod returns the period of final that gateway period id shares
// its origin Period ID with, or nil.
func sameOriginPeriod(final *imdp.ParsedMPD, id string) *imdp.ParsedPeriod {
	origin := index.OriginPeriodID(id)
	for _, p := range final.Periods {
		if index.OriginPeriodID(p.ID) == origin {
			return p
		}
	}
	return nil
}

// renamedPeriod returns a copy of p named id, or nil for a nil p.
func renamedPeriod(p *imdp.ParsedPeriod, id string) *imdp.ParsedPeriod {
	if p == nil {
		return nil
	}
	cp := *p
	cp.ID = id
	return &cp
}
