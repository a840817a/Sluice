package channel

// What a DASH channel in key mode decrypt shows of its decryption: the
// manifest without ContentProtection for the AdaptationSets it decrypted,
// and the warnings that say what stays encrypted.

import (
	"fmt"
	"strings"

	"github.com/a840817a/sluice/internal/dashdecrypt"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

// decryptedView caches withoutDecryptedCP for one parsed MPD and registry
// generation.
type decryptedView struct {
	src        *imdp.ParsedMPD
	generation uint64
	out        *imdp.ParsedMPD
}

// withoutDecryptedCP returns p without the ContentProtection of the
// AdaptationSets rt's registry decrypted, or p itself when the channel has
// no registry or decrypted none of them. The result is cached until p or
// the registry's generation changes.
func (rt *Runtime) withoutDecryptedCP(p *imdp.ParsedMPD) *imdp.ParsedMPD {
	if p == nil || rt.decrypt == nil {
		return p
	}
	gen := rt.decrypt.Generation()
	if v := rt.decryptView.Load(); v != nil && v.src == p && v.generation == gen {
		return v.out
	}
	out := stripDecryptedCP(p, rt.decrypt.IsDecrypted)
	rt.decryptView.Store(&decryptedView{src: p, generation: gen, out: out})
	return out
}

// stripDecryptedCP returns p with ContentProtections, RawCPs and
// ParsedContentProtections cleared on every AdaptationSet decrypted reports
// as decrypted. It copies only the MPD, the periods and the AdaptationSets
// it changes, and returns p itself when it changes none; p is never
// modified.
func stripDecryptedCP(p *imdp.ParsedMPD, decrypted func(dashdecrypt.ASKey) bool) *imdp.ParsedMPD {
	var out *imdp.ParsedMPD
	for pi, period := range p.Periods {
		var periodCopy *imdp.ParsedPeriod
		for ai, as := range period.AdaptationSets {
			if !hasCP(as) || !decrypted(dashdecrypt.ASKey{PeriodID: period.ID, ASID: as.ID}) {
				continue
			}
			if out == nil {
				cp := *p
				cp.Periods = append([]*imdp.ParsedPeriod(nil), p.Periods...)
				out = &cp
			}
			if periodCopy == nil {
				cp := *period
				cp.AdaptationSets = append([]*imdp.ParsedAdaptationSet(nil), period.AdaptationSets...)
				periodCopy = &cp
				out.Periods[pi] = periodCopy
			}
			clearAS := *as
			clearAS.ContentProtections = nil
			clearAS.RawCPs = nil
			clearAS.ParsedContentProtections = nil
			periodCopy.AdaptationSets[ai] = &clearAS
		}
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
