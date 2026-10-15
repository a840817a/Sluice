package dashdecrypt

import "github.com/a840817a/sluice/internal/cenc"

// Lookup returns how to decrypt the media of representation repID of as, and
// false when it is stored as the origin produced it: as is not decrypted, or
// the representation is clear. The KeyLookup answers from the keys the
// registry holds when it is called, so a sample whose group rotates to a KID
// added since is decrypted too.
func (r *Registry) Lookup(as ASKey, repID string) (cenc.TrackProtection, cenc.KeyLookup, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.decrypted[as]
	if !ok {
		return cenc.TrackProtection{}, nil, false
	}
	tp, ok := st.Reps[repID]
	if !ok {
		return cenc.TrackProtection{}, nil, false
	}
	return tp, r.key, true
}
