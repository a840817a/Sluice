package channel

// What a DASH channel in key mode decrypt shows of its decryption: the
// manifest without ContentProtection for the AdaptationSets it decrypted,
// and the warnings that say what stays encrypted.

import (
	"fmt"
	"slices"
	"strings"

	"github.com/a840817a/sluice/internal/dashdecrypt"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

// decryptedView caches decryptView for one parsed MPD and registry
// generation.
type decryptedView struct {
	src        *imdp.ParsedMPD
	generation uint64
	out        *imdp.ParsedMPD
}

// decryptView returns p as rt serves it: without the ContentProtection of
// the AdaptationSets rt's registry decrypted, and without the
// representations it refused (stripDecrypted). It is p itself when the
// channel has no registry or neither applies. The result is cached until p
// or the registry's generation changes.
func (rt *Runtime) decryptView(p *imdp.ParsedMPD) *imdp.ParsedMPD {
	if p == nil || rt.decrypt == nil {
		return p
	}
	gen := rt.decrypt.Generation()
	if v := rt.decryptViewCache.Load(); v != nil && v.src == p && v.generation == gen {
		return v.out
	}
	out := stripDecrypted(p, rt.decrypt)
	rt.decryptViewCache.Store(&decryptedView{src: p, generation: gen, out: out})
	return out
}

// decryptState is what stripDecrypted asks of a registry.
type decryptState interface {
	IsDecrypted(as dashdecrypt.ASKey) bool
	IsRefused(as dashdecrypt.ASKey, repID string) bool
}

// stripDecrypted returns p with ContentProtections, RawCPs and
// ParsedContentProtections cleared on every AdaptationSet st reports as
// decrypted, and without the representations of those that st reports as
// refused: their init was never stored, so a player switching to one would
// find nothing. An AdaptationSet left with no representation is left out.
// It copies only the MPD, the periods and the AdaptationSets it changes,
// and returns p itself when it changes none; p is never modified.
func stripDecrypted(p *imdp.ParsedMPD, st decryptState) *imdp.ParsedMPD {
	var out *imdp.ParsedMPD
	for pi, period := range p.Periods {
		var kept []*imdp.ParsedAdaptationSet
		changed := false
		for _, as := range period.AdaptationSets {
			key := dashdecrypt.ASKey{PeriodID: period.ID, ASID: as.ID}
			if !st.IsDecrypted(key) {
				kept = append(kept, as)
				continue
			}
			reps := slices.DeleteFunc(slices.Clone(as.Representations), func(r *imdp.ParsedRepresentation) bool {
				return st.IsRefused(key, r.ID)
			})
			if !hasCP(as) && len(reps) == len(as.Representations) {
				kept = append(kept, as)
				continue
			}
			changed = true
			if len(reps) == 0 {
				continue
			}
			clearAS := *as
			clearAS.ContentProtections = nil
			clearAS.RawCPs = nil
			clearAS.ParsedContentProtections = nil
			clearAS.Representations = reps
			kept = append(kept, &clearAS)
		}
		if !changed {
			continue
		}
		if out == nil {
			cp := *p
			cp.Periods = append([]*imdp.ParsedPeriod(nil), p.Periods...)
			out = &cp
		}
		periodCopy := *period
		periodCopy.AdaptationSets = kept
		out.Periods[pi] = &periodCopy
	}
	if out == nil {
		return p
	}
	return out
}

func hasCP(as *imdp.ParsedAdaptationSet) bool {
	return len(as.ContentProtections) > 0 || len(as.RawCPs) > 0 || len(as.ParsedContentProtections) > 0
}

// decryptWarnings is what status reports for a DASH decrypt channel: the
// registry's warnings, then hls_closed for each AdaptationSet of parsed,
// the MPD the channel serves from, that still carries ContentProtection
// while the channel is not served as HLS. It is nil for any other channel.
func (rt *Runtime) decryptWarnings(parsed *imdp.ParsedMPD, servesHLS bool) []dashdecrypt.Warning {
	if rt.decrypt == nil {
		return nil
	}
	out := rt.decrypt.Warnings()
	if servesHLS || parsed == nil {
		return out
	}
	for _, period := range parsed.Periods {
		for _, as := range period.AdaptationSets {
			if !hasCP(as) {
				continue
			}
			var kids []string
			for _, cp := range as.ParsedContentProtections {
				if cp.DefaultKID != "" {
					kids = append(kids, strings.ToLower(strings.ReplaceAll(cp.DefaultKID, "-", "")))
					break
				}
			}
			out = append(out, dashdecrypt.Warning{
				Period: period.ID, AdaptationSet: as.ID, Kind: dashdecrypt.KindHLSClosed, KIDs: kids,
				Message: fmt.Sprintf("HLS is not served: %s AdaptationSet %s is still encrypted", as.MediaType, as.ID),
			})
		}
	}
	return out
}
