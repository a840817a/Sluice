package dashdecrypt

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/a840817a/sluice/internal/cenc"
)

// Warning kinds.
const (
	// KindMissingKey: an AdaptationSet stays encrypted because the channel
	// holds no key for one of its KIDs, which the warning lists; or, in a
	// decrypted AdaptationSet, a representation is refused or segments whose
	// samples rotate to one of those KIDs are withheld until its key is added.
	KindMissingKey = "missing_key"
	// KindUnsupportedScheme: an AdaptationSet stays encrypted because a
	// representation uses a scheme or feature internal/cenc does not decrypt
	// (cens, cbc1, a malformed seig sample group, several encrypted tracks in
	// one init).
	KindUnsupportedScheme = "unsupported_scheme"
	// KindMalformed: an init or segment could not be read, so the
	// AdaptationSet stays encrypted or a segment was dropped.
	KindMalformed = "malformed"
	// KindKeyUnmatched: a key whose KID no init or decrypted segment of the
	// channel has used so far. A key used only by the seig sample groups of
	// later segments is listed until one of them decrypts with it. Reported
	// only once the channel has read an init.
	KindKeyUnmatched = "key_unmatched"
	// KindConversionFailed: converting an AdaptationSet stored as ciphertext
	// to clear, once keys were added, failed or was refused, so it stays
	// encrypted until keys are added again.
	KindConversionFailed = "conversion_failed"
	// KindHLSClosed: the channel is not served as HLS because an
	// AdaptationSet still carries ContentProtection.
	KindHLSClosed = "hls_closed"
)

// Warning says why part of a DASH decrypt channel is not decrypted, or not
// served as HLS. Period and AdaptationSet are empty for a channel-level
// warning. KIDs are hex; a warning never holds a key.
type Warning struct {
	Period        string   `json:"period"`
	AdaptationSet string   `json:"adaptation_set,omitempty"`
	Kind          string   `json:"kind"`
	KIDs          []string `json:"kids,omitempty"`
	Message       string   `json:"message"`
}

// Warnings returns the registry's warnings, AdaptationSet ones in period and
// AdaptationSet order, then key_unmatched.
func (r *Registry) Warnings() []Warning {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Warning, 0, len(r.warnings)+1)
	for _, w := range r.warnings {
		w.KIDs = slices.Clone(w.KIDs)
		out = append(out, w)
	}
	slices.SortFunc(out, func(a, b Warning) int {
		return strings.Compare(a.Period+"\x00"+a.AdaptationSet+"\x00"+a.Kind, b.Period+"\x00"+b.AdaptationSet+"\x00"+b.Kind)
	})
	if len(r.seen) > 0 {
		var unmatched []string
		for kid := range r.keys {
			if !r.seen[kid] {
				unmatched = append(unmatched, kid.String())
			}
		}
		if len(unmatched) > 0 {
			slices.Sort(unmatched)
			out = append(out, Warning{Kind: KindKeyUnmatched, KIDs: unmatched,
				Message: "no init or segment seen so far uses these keys"})
		}
	}
	return out
}

// repInit is one representation's init segment, as the origin serves it.
type repInit struct {
	repID string
	data  []byte
}

// decision is what decide makes of an AdaptationSet's inits.
type decision struct {
	// reps holds each encrypted representation's protection. It is empty for
	// a clear AdaptationSet.
	reps map[string]cenc.TrackProtection
	// warning, when set, says why the AdaptationSet cannot be decrypted.
	warning *Warning
	// missing lists the KIDs of a missing_key warning.
	missing []cenc.KID
	// kids lists every KID the inits carry.
	kids []cenc.KID
}

// decide works out whether an AdaptationSet whose representations have
// inits can be decrypted with keys: every encrypted representation has one
// encrypted track, in a scheme internal/cenc decrypts, with a key held for
// each KID its init names (tenc's and those of its seig sample groups).
func decide(inits []repInit, keys cenc.KeySet) decision {
	d := decision{reps: make(map[string]cenc.TrackProtection)}
	var missing []cenc.KID
	var fail *Warning
	for _, in := range inits {
		tps, err := cenc.ReadInitProtection(in.data)
		if err != nil {
			if fail == nil {
				kind := KindMalformed
				if errors.Is(err, cenc.ErrUnsupportedScheme) {
					kind = KindUnsupportedScheme
				}
				fail = &Warning{Kind: kind, Message: fmt.Sprintf("representation %s: %v", in.repID, err)}
			}
			continue
		}
		for _, tp := range tps {
			for _, kid := range tp.KIDs() {
				if !slices.Contains(d.kids, kid) {
					d.kids = append(d.kids, kid)
				}
			}
		}
		switch len(tps) {
		case 0:
			continue
		case 1:
		default:
			if fail == nil {
				fail = &Warning{Kind: KindUnsupportedScheme,
					Message: fmt.Sprintf("representation %s has %d encrypted tracks", in.repID, len(tps))}
			}
			continue
		}
		tp := tps[0]
		d.reps[in.repID] = tp
		for _, kid := range tp.KIDs() {
			if _, ok := keys[kid]; !ok && !slices.Contains(missing, kid) {
				missing = append(missing, kid)
			}
		}
	}
	switch {
	case fail != nil:
		d.warning = fail
	case len(missing) > 0:
		slices.SortFunc(missing, func(a, b cenc.KID) int { return bytes.Compare(a[:], b[:]) })
		d.missing = missing
		w := missingKeyWarning(missing, nil, nil)
		d.warning = &w
	}
	if d.warning != nil {
		d.reps = nil
	}
	return d
}

// missingKeyWarning is the missing_key warning for kids, which have no key
// held, whatever lacks them: an AdaptationSet stored as ciphertext, or a
// decrypted one whose representations refused names, and whose segments
// from representations withheld names, need them.
func missingKeyWarning(kids []cenc.KID, refused, withheld []string) Warning {
	kids = slices.Clone(kids)
	slices.SortFunc(kids, func(a, b cenc.KID) int { return bytes.Compare(a[:], b[:]) })
	msg := "no content key held for these KIDs"
	if len(refused) > 0 {
		msg += "; representations refused: " + strings.Join(slices.Sorted(slices.Values(refused)), ", ")
	}
	if len(withheld) > 0 {
		msg += "; segments that use them withheld from representations: " + strings.Join(slices.Sorted(slices.Values(withheld)), ", ")
	}
	return Warning{Kind: KindMissingKey, KIDs: kidStrings(kids), Message: msg}
}

// kidStrings returns kids in hex, as a Warning lists them.
func kidStrings(kids []cenc.KID) []string {
	out := make([]string, len(kids))
	for i, k := range kids {
		out[i] = k.String()
	}
	return out
}
