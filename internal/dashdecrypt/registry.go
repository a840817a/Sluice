// Package dashdecrypt decides which AdaptationSets of a DASH channel in key
// mode decrypt are decrypted during ingest, and remembers the decision
// (ADR-0010).
//
// The unit is one AdaptationSet of one period. It is decrypted only when the
// channel holds a key for the KID in the tenc box of every one of its
// representations' init segments, and for every KID their seig sample groups
// name, and the scheme is one internal/cenc decrypts; otherwise every
// representation of it stays ciphertext, as a passthrough channel stores it,
// and the reason is reported as a Warning.
//
// The decision is stored in the AdaptationSet's directory as decrypt.json,
// which holds each encrypted representation's cenc.TrackProtection, the
// representations that joined later and were refused with the KIDs they
// lacked, the KIDs whose segments were withheld, and never a key. The file
// exists exactly when the AdaptationSet is decrypted: it is written before the
// first clear init, or, for an AdaptationSet first stored as ciphertext,
// swapped in with the files Convert rewrote once its keys arrived. It is read
// back when the channel starts.
//
// Content keys never appear in a log line, a warning or an error.
package dashdecrypt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a840817a/sluice/internal/cenc"
	"github.com/a840817a/sluice/internal/queue"
	"github.com/a840817a/sluice/internal/store"
)

// StateFileName is the file in an AdaptationSet directory that records it as
// decrypted.
const StateFileName = "decrypt.json"

// ASKey names one AdaptationSet of a channel: the gateway period ID
// (ADR-0007) and the AdaptationSet ID the parser gives it, which is its
// position in the period. Tasks, the index, the stored layout and the
// gateway's parsed MPD all use these IDs.
type ASKey struct {
	PeriodID string
	ASID     string
}

// asState is the content of decrypt.json.
type asState struct {
	// Reps holds the protection of each encrypted representation. A clear
	// representation of a decrypted AdaptationSet has no entry.
	Reps map[string]cenc.TrackProtection `json:"reps"`
	// URLVersion is set when a conversion rewrote files players could have
	// cached as ciphertext (Registry.URLVersion), and 0 for an AdaptationSet
	// decrypted from its first file.
	URLVersion uint64 `json:"url_version,omitempty"`
	// Refused lists the representations that joined the AdaptationSet after
	// its decision and could not be decrypted, so their init was refused
	// (addRep). They are not served until a later init of theirs is
	// accepted.
	Refused []string `json:"refused,omitempty"`
	// RefusedKIDs holds, per refused representation, the KIDs it was refused
	// for want of: empty for one refused for another reason, such as its
	// scheme. A representation refused by a gateway that did not record
	// them has no entry, and is retried whenever any key is added.
	RefusedKIDs map[string][]cenc.KID `json:"refused_kids,omitempty"`
	// Withheld lists, per representation, the KIDs its segments rotated to
	// with no key held, so those segments were dropped unstored
	// (DecryptSegment). A KID leaves it once a segment of the
	// representation decrypts with its key. It is kept on disk so a restart
	// still reports the missing_key warning and knows what to retry when
	// the key arrives.
	Withheld map[string][]cenc.KID `json:"withheld,omitempty"`
}

// FetchInit fetches the init segment ref locates from the origin.
type FetchInit func(ctx context.Context, ref queue.InitRef) ([]byte, error)

// Registry is one channel's decryption state. It is safe for concurrent use.
type Registry struct {
	dataDir, channelID string
	// generation changes whenever an AdaptationSet becomes decrypted or a
	// representation is refused or accepted (IsRefused), so a view derived
	// from them can be cached against it.
	generation atomic.Uint64
	// keyEpoch counts the AddKeys calls.
	keyEpoch atomic.Uint64

	mu   sync.RWMutex
	keys cenc.KeySet
	// decrypted holds the AdaptationSets with a decrypt.json.
	decrypted map[ASKey]asState
	// kept holds the AdaptationSets this run decided to store as the origin
	// produced them. It closes the window between that decision and the first
	// stored file, which the on-disk check cannot see.
	kept map[ASKey]bool
	// warnings holds one warning per AdaptationSet and kind.
	warnings map[warningKey]Warning
	// seen is every KID an init of this channel has carried.
	seen map[cenc.KID]bool
	// failed holds the AdaptationSets whose conversion failed, which
	// Convertible leaves out, with the KIDs whose missing keys failed it.
	// AddKeys ends an entry when it adds a key for one of those KIDs, or
	// adds any key for an entry that lists none.
	failed map[ASKey][]cenc.KID
	// wake is Wake's channel.
	wake chan struct{}

	locksMu sync.Mutex
	locks   map[ASKey]*sync.Mutex
	// initLocks keep a conversion's swap from overlapping the storing of an
	// init of the same AdaptationSet (HoldInit).
	initLocks map[ASKey]*sync.RWMutex
}

type warningKey struct {
	as   ASKey
	kind string
}

// NewRegistry returns the registry of channelID, whose media lives under
// dataDir, decrypting with keys. It knows no decision until Load.
func NewRegistry(dataDir, channelID string, keys cenc.KeySet) *Registry {
	r := &Registry{
		dataDir:   dataDir,
		channelID: channelID,
		keys:      make(cenc.KeySet, len(keys)),
		decrypted: make(map[ASKey]asState),
		kept:      make(map[ASKey]bool),
		warnings:  make(map[warningKey]Warning),
		seen:      make(map[cenc.KID]bool),
		failed:    make(map[ASKey][]cenc.KID),
		wake:      make(chan struct{}, 1),
		locks:     make(map[ASKey]*sync.Mutex),
		initLocks: make(map[ASKey]*sync.RWMutex),
	}
	for kid, k := range keys {
		r.keys[kid] = k
	}
	return r
}

// AddKeys gives the registry more keys. AdaptationSets decided from now on
// can use them, and one already stored as ciphertext can be converted
// (Convertible), including one whose conversion failed before for want of
// one of them, or for a reason no KID names; Wake says so. A KID they cover
// leaves the missing_key warnings at once, though the segments withheld for
// it are stored only when fetched again (Retriable). It returns the KIDs of
// keys.
func (r *Registry) AddKeys(keys []cenc.Key) []cenc.KID {
	added := make([]cenc.KID, len(keys))
	for i, k := range keys {
		added[i] = k.KID
	}
	r.mu.Lock()
	for _, k := range keys {
		r.keys[k.KID] = k.Value
	}
	for as, missing := range r.failed {
		if len(missing) == 0 || intersects(missing, added) {
			delete(r.failed, as)
			for _, kid := range added {
				r.dropMissingKID(as, kid)
			}
		}
	}
	for as := range r.decrypted {
		r.syncMissing(as)
	}
	r.mu.Unlock()
	r.keyEpoch.Add(1)
	r.nudge()
	return added
}

// intersects reports whether any KID of kids is in set.
func intersects(kids, set []cenc.KID) bool {
	return slices.ContainsFunc(kids, func(k cenc.KID) bool { return slices.Contains(set, k) })
}

// KeyEpoch changes each time keys are added. An init whose preparation
// started under an earlier epoch may have failed for want of a key it can
// now use.
func (r *Registry) KeyEpoch() uint64 { return r.keyEpoch.Load() }

// Generation identifies the current set of decrypted AdaptationSets and of
// refused representations.
func (r *Registry) Generation() uint64 { return r.generation.Load() }

// IsRefused reports whether representation repID of the decrypted
// AdaptationSet as had its init refused and none accepted since: it
// joined after the AdaptationSet was decided, with a KID the channel holds
// no key for or a scheme it cannot decrypt, so nothing of it is stored.
func (r *Registry) IsRefused(as ASKey, repID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Contains(r.decrypted[as].Refused, repID)
}

// RefusedRep names one representation of an AdaptationSet.
type RefusedRep struct {
	AS    ASKey
	RepID string
}

// Retriable lists the representations of decrypted AdaptationSets worth
// fetching again now that the keys of added are held: those whose init was
// refused (IsRefused) or whose segments were withheld for want of one of
// them, and those whose refusal decrypt.json records without its KIDs. A
// representation refused for another reason, such as its scheme, is not
// listed.
func (r *Registry) Retriable(added []cenc.KID) []RefusedRep {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []RefusedRep
	for as, st := range r.decrypted {
		for _, id := range st.Refused {
			if kids, known := st.RefusedKIDs[id]; !known || intersects(kids, added) {
				out = append(out, RefusedRep{AS: as, RepID: id})
			}
		}
		for id, kids := range st.Withheld {
			rep := RefusedRep{AS: as, RepID: id}
			if intersects(kids, added) && !slices.Contains(out, rep) {
				out = append(out, rep)
			}
		}
	}
	return out
}

// IsDecrypted reports whether the AdaptationSet as is decrypted.
func (r *Registry) IsDecrypted(as ASKey) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.decrypted[as]
	return ok
}

// key is the registry's cenc.KeyLookup.
func (r *Registry) key(kid cenc.KID) ([16]byte, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	k, ok := r.keys[kid]
	return k, ok
}

// ASLock serializes the decisions about as. It returns the unlock function.
func (r *Registry) ASLock(as ASKey) func() {
	r.locksMu.Lock()
	l := r.locks[as]
	if l == nil {
		l = new(sync.Mutex)
		r.locks[as] = l
	}
	r.locksMu.Unlock()
	l.Lock()
	return l.Unlock
}

// HoldInit keeps a conversion of as from swapping its directory until the
// returned function is called. Whoever stores an init of as holds it from
// PrepareInit until the init is written, so the init lands in the directory
// the decision was made for.
func (r *Registry) HoldInit(as ASKey) func() {
	l := r.initLock(as)
	l.RLock()
	return l.RUnlock
}

// excludeInits waits for every HoldInit of as to end and holds off new ones
// until the returned function is called.
func (r *Registry) excludeInits(as ASKey) func() {
	l := r.initLock(as)
	l.Lock()
	return l.Unlock
}

func (r *Registry) initLock(as ASKey) *sync.RWMutex {
	r.locksMu.Lock()
	defer r.locksMu.Unlock()
	l := r.initLocks[as]
	if l == nil {
		l = new(sync.RWMutex)
		r.initLocks[as] = l
	}
	return l
}

// Load reads the decision of every AdaptationSet stored on disk: a
// decrypt.json makes it decrypted, and the inits of its representations are
// cleared again, which repairs a crash between writing the file and the
// clear init. An AdaptationSet without one keeps what it stores, and an
// encrypted one is reported as a warning.
//
// Only a failure to list the channel's directories is returned; a damaged
// AdaptationSet is logged and reported as a warning.
func (r *Registry) Load() error {
	periods, err := store.ScanChannel(r.dataDir, r.channelID)
	if err != nil {
		return err
	}
	for _, p := range periods {
		for _, as := range p.AdaptationSets {
			if len(as.Reps) == 0 {
				continue
			}
			r.loadAS(ASKey{PeriodID: p.ID, ASID: as.ID}, as)
		}
	}
	r.generation.Add(1)
	return nil
}

func (r *Registry) loadAS(key ASKey, as store.ASDir) {
	statePath := filepath.Join(as.Reps[0].ASDir(), StateFileName)
	raw, err := os.ReadFile(statePath)
	if err == nil {
		var st asState
		if err := json.Unmarshal(raw, &st); err != nil {
			slog.Error("decrypt state unreadable; the AdaptationSet is kept as stored",
				"channel", r.channelID, "period", key.PeriodID, "as", key.ASID, "err", err)
			r.record(key, Warning{Kind: KindMalformed, Message: fmt.Sprintf("%s unreadable: %v", StateFileName, err)})
			return
		}
		if st.Reps == nil {
			st.Reps = make(map[string]cenc.TrackProtection)
		}
		r.mu.Lock()
		// A KID whose key arrived while the channel was stopped needs no
		// retry: this run's ingest offers the withheld segments again, as
		// for any segment not stored. Its file entry goes with the next
		// write.
		for id, kids := range st.Withheld {
			r.markSeen(kids)
			kids = slices.DeleteFunc(kids, func(k cenc.KID) bool { _, ok := r.keys[k]; return ok })
			if len(kids) == 0 {
				delete(st.Withheld, id)
			} else {
				st.Withheld[id] = kids
			}
		}
		r.decrypted[key] = st
		for _, tp := range st.Reps {
			r.markSeen(tp.KIDs())
		}
		for _, kids := range st.RefusedKIDs {
			r.markSeen(kids)
		}
		r.syncMissing(key)
		r.mu.Unlock()
		for _, rep := range as.Reps {
			if _, ok := st.Reps[rep.RepID]; ok {
				r.reclearInit(rep.InitPath())
			}
		}
		return
	}
	if !errors.Is(err, os.ErrNotExist) {
		slog.Warn("decrypt state unreadable", "channel", r.channelID, "path", statePath, "err", err)
	}

	// Stored as the origin produced it: say why, if it is encrypted.
	var inits []repInit
	for _, rep := range as.Reps {
		data, err := os.ReadFile(rep.InitPath())
		if err != nil {
			continue
		}
		inits = append(inits, repInit{repID: rep.RepID, data: data})
	}
	r.mu.Lock()
	r.kept[key] = true
	d := decide(inits, r.keys)
	r.markSeen(d.kids)
	r.mu.Unlock()
	if d.warning != nil {
		r.record(key, *d.warning)
	}
}

// reclearInit rewrites the init at path as a clear one if it is not already.
func (r *Registry) reclearInit(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	clear, err := cenc.ClearInit(data)
	if err != nil {
		slog.Warn("decrypted AdaptationSet has an init that cannot be cleared", "channel", r.channelID, "path", path, "err", err)
		return
	}
	if bytes.Equal(clear, data) {
		return
	}
	if err := store.Write(path, clear); err != nil {
		slog.Warn("rewrite clear init failed", "channel", r.channelID, "path", path, "err", err)
	}
}

// PrepareInit returns the bytes to store as the init of task's
// representation, given orig, the init as the origin serves it. It decides
// task's AdaptationSet if nothing has yet:
//
//   - a decrypted AdaptationSet gets a clear init; a representation the
//     decision did not cover is added to it if a key is held for its KID,
//     and fails otherwise, so the AdaptationSet never mixes the two;
//   - one that already stores media or inits (held is true, or a
//     representation directory of it is not empty), or that this run
//     already decided to keep, gets orig, and Wake delivers when orig's
//     KID has a key or orig is clear, since the stored AdaptationSet may
//     be convertible now that this init is stored;
//   - a new one gets every representation's init (fetch, for those of
//     task.ASInits other than task's own) and is decrypted when every KID has
//     a key and every scheme is supported: decrypt.json is written before the
//     clear init is returned. Otherwise it is kept and a warning recorded.
func (r *Registry) PrepareInit(ctx context.Context, task *queue.SegmentTask, orig []byte, held bool, fetch FetchInit) ([]byte, error) {
	key := ASKeyOf(task)
	defer r.ASLock(key)()

	r.mu.RLock()
	st, decrypted := r.decrypted[key]
	kept := r.kept[key]
	r.mu.RUnlock()

	if decrypted {
		if _, ok := st.Reps[task.RepID]; !ok {
			if err := r.addRep(key, task, st, orig); err != nil {
				return nil, err
			}
		}
		return cenc.ClearInit(orig)
	}
	loc := location(r.dataDir, r.channelID, task)
	if kept || held || repDirsHoldFiles(loc.ASDir()) {
		r.keep(key, []repInit{{repID: task.RepID, data: orig}})
		// Stored as ciphertext, but perhaps convertible now.
		r.mu.RLock()
		d := decide([]repInit{{repID: task.RepID, data: orig}}, r.keys)
		r.mu.RUnlock()
		// A clear init can complete one too: every representation's init
		// must be stored (Convertible).
		if d.warning == nil {
			r.nudge()
		}
		return orig, nil
	}

	inits := []repInit{{repID: task.RepID, data: orig}}
	for _, ref := range task.ASInits {
		if ref.RepID == task.RepID {
			continue
		}
		data, err := fetchWithRetry(ctx, fetch, ref)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			r.keep(key, inits)
			r.record(key, Warning{Kind: KindMalformed,
				Message: fmt.Sprintf("init of representation %s unavailable, so the AdaptationSet is kept encrypted: %v", ref.RepID, err)})
			return orig, nil
		}
		inits = append(inits, repInit{repID: ref.RepID, data: data})
	}

	r.mu.RLock()
	d := decide(inits, r.keys)
	r.mu.RUnlock()
	if d.warning != nil || len(d.reps) == 0 {
		r.keep(key, nil)
		r.mu.Lock()
		r.markSeen(d.kids)
		r.mu.Unlock()
		if d.warning != nil {
			r.record(key, *d.warning)
		}
		return orig, nil
	}
	st = asState{Reps: d.reps}
	if err := writeState(loc.ASDir(), st); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.decrypted[key] = st
	r.markSeen(d.kids)
	r.mu.Unlock()
	r.generation.Add(1)
	slog.Info("decrypting AdaptationSet during ingest", "channel", r.channelID,
		"period", key.PeriodID, "as", key.ASID, "reps", len(st.Reps))
	return cenc.ClearInit(orig)
}

// addRep adds a representation the decision of decrypted AdaptationSet key
// did not cover. One it cannot decrypt is refused: recorded in
// decrypt.json, so it stays hidden across a restart, with a warning. One it
// can is accepted, which ends an earlier refusal and its part of the
// missing_key warning. Caller holds key's AS lock.
func (r *Registry) addRep(key ASKey, task *queue.SegmentTask, st asState, orig []byte) error {
	r.mu.RLock()
	d := decide([]repInit{{repID: task.RepID, data: orig}}, r.keys)
	r.mu.RUnlock()
	r.mu.Lock()
	r.markSeen(d.kids)
	r.mu.Unlock()
	asDir := location(r.dataDir, r.channelID, task).ASDir()
	wasRefused := slices.Contains(st.Refused, task.RepID)
	if d.warning != nil {
		refusal := fmt.Errorf("representation %s cannot join decrypted AdaptationSet %s: %s", task.RepID, key.ASID, d.warning.Message)
		// An empty list, not none: it says the refusal names no KID.
		missing := append([]cenc.KID{}, d.missing...)
		next := st
		var err error
		if kids, known := st.RefusedKIDs[task.RepID]; !wasRefused || !known || !slices.Equal(kids, missing) {
			next = st.clone()
			if !wasRefused {
				next.Refused = append(next.Refused, task.RepID)
			}
			if next.RefusedKIDs == nil {
				next.RefusedKIDs = make(map[string][]cenc.KID)
			}
			next.RefusedKIDs[task.RepID] = missing
			if err = writeState(asDir, next); err != nil {
				// The refusal still holds; only a restart forgets it.
				slog.Warn("refused representation not recorded on disk", "channel", r.channelID,
					"period", key.PeriodID, "as", key.ASID, "rep", task.RepID, "err", err)
			}
		}
		r.mu.Lock()
		r.decrypted[key] = next
		// The warning is recorded with the refusal, so whoever sees it also
		// sees the representation hidden.
		if d.warning.Kind == KindMissingKey {
			r.syncMissing(key)
		} else {
			r.recordLocked(key, *d.warning)
		}
		r.mu.Unlock()
		if !wasRefused {
			r.generation.Add(1)
		}
		return errors.Join(refusal, err)
	}
	tp, encrypted := d.reps[task.RepID]
	if !encrypted && !wasRefused {
		return nil // a clear representation needs no entry
	}
	next := st.clone()
	if encrypted {
		next.Reps[task.RepID] = tp
	}
	next.Refused = slices.DeleteFunc(next.Refused, func(id string) bool { return id == task.RepID })
	delete(next.RefusedKIDs, task.RepID)
	if len(next.RefusedKIDs) == 0 {
		next.RefusedKIDs = nil
	}
	if err := writeState(asDir, next); err != nil {
		return err
	}
	r.mu.Lock()
	r.decrypted[key] = next
	if wasRefused {
		r.generation.Add(1)
		r.syncMissing(key)
	}
	r.mu.Unlock()
	return nil
}

// syncMissing rebuilds the missing_key warning of decrypted AdaptationSet
// key from what still needs a key it lacks: its refused representations and
// its withheld segments. A KID is listed while either needs it and no key
// for it is held; the warning goes once none is listed. Caller holds r.mu.
func (r *Registry) syncMissing(key ASKey) {
	st, ok := r.decrypted[key]
	if !ok {
		return
	}
	var kids []cenc.KID
	var refused, withheld []string
	note := func(ks []cenc.KID) bool {
		lacking := false
		for _, k := range ks {
			if _, held := r.keys[k]; held {
				continue
			}
			lacking = true
			if !slices.Contains(kids, k) {
				kids = append(kids, k)
			}
		}
		return lacking
	}
	for id, ks := range st.RefusedKIDs {
		if slices.Contains(st.Refused, id) && note(ks) {
			refused = append(refused, id)
		}
	}
	for id, ks := range st.Withheld {
		if note(ks) {
			withheld = append(withheld, id)
		}
	}
	if len(kids) == 0 {
		delete(r.warnings, warningKey{as: key, kind: KindMissingKey})
		return
	}
	r.recordLocked(key, missingKeyWarning(kids, refused, withheld))
}

// dropMissingKID takes kid out of key's missing_key warning, and drops the
// warning once it names no KID. Caller holds r.mu.
func (r *Registry) dropMissingKID(key ASKey, kid cenc.KID) {
	wk := warningKey{as: key, kind: KindMissingKey}
	w, ok := r.warnings[wk]
	if !ok {
		return
	}
	w.KIDs = slices.DeleteFunc(slices.Clone(w.KIDs), func(k string) bool { return k == kid.String() })
	if len(w.KIDs) == 0 {
		delete(r.warnings, wk)
		return
	}
	r.warnings[wk] = w
}

// clone returns a copy of st that shares nothing with it.
func (st asState) clone() asState {
	out := asState{Reps: make(map[string]cenc.TrackProtection, len(st.Reps)+1), URLVersion: st.URLVersion,
		Refused: slices.Clone(st.Refused), RefusedKIDs: cloneKIDLists(st.RefusedKIDs),
		Withheld: cloneKIDLists(st.Withheld)}
	for id, p := range st.Reps {
		out.Reps[id] = p
	}
	return out
}

// cloneKIDLists returns a copy of m that shares nothing with it, or nil for
// an empty m.
func cloneKIDLists(m map[string][]cenc.KID) map[string][]cenc.KID {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string][]cenc.KID, len(m))
	for id, kids := range m {
		out[id] = slices.Clone(kids)
	}
	return out
}

// keep records that key is stored as the origin produced it, noting the KIDs
// inits carry.
func (r *Registry) keep(key ASKey, inits []repInit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kept[key] = true
	for _, in := range inits {
		if tps, err := cenc.ReadInitProtection(in.data); err == nil {
			for _, tp := range tps {
				r.markSeen(tp.KIDs())
			}
		}
	}
}

// markSeen records kids as used by this channel's inits. Caller holds r.mu.
func (r *Registry) markSeen(kids []cenc.KID) {
	for _, k := range kids {
		r.seen[k] = true
	}
}

// DecryptSegment decrypts, in place, the media segment seg of task's
// representation when its AdaptationSet is decrypted, and leaves it as it
// is otherwise. Each sample takes the key of the KID its seig sample group
// or tenc names from the keys held now, so a sample whose group rotates to
// a KID added since is decrypted too.
//
// A segment that cannot be decrypted must not be stored, since its
// AdaptationSet is clear; the error says why, and a warning records it. One
// whose samples rotate to KIDs without a key is withheld: those KIDs join
// the AdaptationSet's missing_key warning and its decrypt.json, and the
// representation is Retriable once one of their keys is added, until a
// segment of it decrypts with that key.
func (r *Registry) DecryptSegment(task *queue.SegmentTask, seg []byte) error {
	as := ASKeyOf(task)
	r.mu.RLock()
	tp, ok := r.decrypted[as].Reps[task.RepID]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	// DecryptSegment looks each KID up once.
	var used []cenc.KID
	err := cenc.DecryptSegment(seg, tp, func(kid cenc.KID) ([16]byte, bool) {
		k, ok := r.key(kid)
		if ok {
			used = append(used, kid)
		}
		return k, ok
	})
	if err != nil {
		r.segmentFailed(task, err)
		return err
	}
	r.segmentDecrypted(task, used)
	return nil
}

// segmentFailed records why a segment of task's representation was not
// decrypted.
func (r *Registry) segmentFailed(task *queue.SegmentTask, err error) {
	as := ASKeyOf(task)
	mk := (*cenc.MissingKeyError)(nil)
	if !errors.As(err, &mk) {
		kind := KindMalformed
		if errors.Is(err, cenc.ErrUnsupportedScheme) {
			kind = KindUnsupportedScheme
		}
		r.record(as, Warning{Kind: kind,
			Message: fmt.Sprintf("segment %d of representation %s not decrypted: %v", task.SegNo, task.RepID, err)})
		return
	}
	r.updateWithheld(task, func(kids []cenc.KID) []cenc.KID {
		for _, kid := range mk.KIDs {
			if !slices.Contains(kids, kid) {
				kids = append(kids, kid)
			}
		}
		return kids
	}, mk.KIDs)
}

// segmentDecrypted reports that a segment of task's representation was
// decrypted with the keys of kids, which a fragment's own seig sample group
// can name though no init does: they count as used (key_unmatched). A KID
// the representation's segments were withheld for leaves its withheld list.
func (r *Registry) segmentDecrypted(task *queue.SegmentTask, kids []cenc.KID) {
	r.updateWithheld(task, func(held []cenc.KID) []cenc.KID {
		return slices.DeleteFunc(held, func(k cenc.KID) bool { return slices.Contains(kids, k) })
	}, kids)
}

// updateWithheld replaces the withheld KIDs of task's representation with
// what change makes of a copy of them, marks seen as used, and rewrites
// decrypt.json and the missing_key warning when the list changed. change
// may be called more than once.
func (r *Registry) updateWithheld(task *queue.SegmentTask, change func([]cenc.KID) []cenc.KID, seen []cenc.KID) {
	as := ASKeyOf(task)
	// plan returns as's state and what change makes of the withheld list,
	// and whether that differs from it. Caller holds r.mu.
	plan := func() (asState, []cenc.KID, bool) {
		st, ok := r.decrypted[as]
		if !ok {
			return st, nil, false
		}
		before := st.Withheld[task.RepID]
		after := change(slices.Clone(before))
		slices.SortFunc(after, func(a, b cenc.KID) int { return bytes.Compare(a[:], b[:]) })
		return st, after, !slices.Equal(before, after)
	}
	// The common case, every KID seen and the list unchanged, takes one
	// read lock.
	r.mu.RLock()
	_, _, changed := plan()
	unseen := slices.ContainsFunc(seen, func(k cenc.KID) bool { return !r.seen[k] })
	r.mu.RUnlock()
	if !changed && !unseen {
		return
	}
	// Every writer of a decrypted AdaptationSet's decrypt.json holds its
	// lock, so the state read under it stays current until written.
	defer r.ASLock(as)()
	r.mu.Lock()
	r.markSeen(seen)
	st, after, changed := plan()
	r.mu.Unlock()
	if !changed {
		return
	}
	next := st.clone()
	if next.Withheld == nil {
		next.Withheld = make(map[string][]cenc.KID)
	}
	if len(after) == 0 {
		delete(next.Withheld, task.RepID)
	} else {
		next.Withheld[task.RepID] = after
	}
	if err := writeState(location(r.dataDir, r.channelID, task).ASDir(), next); err != nil {
		// The warning still says what is withheld; only a restart forgets.
		slog.Warn("withheld segments not recorded on disk", "channel", r.channelID,
			"period", as.PeriodID, "as", as.ASID, "rep", task.RepID, "err", err)
	}
	r.mu.Lock()
	r.decrypted[as] = next
	r.syncMissing(as)
	r.mu.Unlock()
}

func (r *Registry) record(as ASKey, w Warning) {
	r.mu.Lock()
	r.recordLocked(as, w)
	r.mu.Unlock()
}

// recordLocked sets as's warning of w's kind. Caller holds r.mu.
func (r *Registry) recordLocked(as ASKey, w Warning) {
	w.Period, w.AdaptationSet = as.PeriodID, as.ASID
	r.warnings[warningKey{as: as, kind: w.Kind}] = w
}

// A sibling init is fetched this many times, this far apart at first.
const siblingAttempts = 3

var siblingBackoff = time.Second

func fetchWithRetry(ctx context.Context, fetch FetchInit, ref queue.InitRef) ([]byte, error) {
	if fetch == nil {
		return nil, errors.New("no init fetcher")
	}
	backoff := siblingBackoff
	for attempt := 1; ; attempt++ {
		data, err := fetch(ctx, ref)
		if err == nil || attempt >= siblingAttempts || ctx.Err() != nil {
			return data, err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		backoff *= 2
	}
}

// ASKeyOf names the AdaptationSet of task's representation.
func ASKeyOf(task *queue.SegmentTask) ASKey {
	return ASKey{PeriodID: task.PeriodID, ASID: task.ASID}
}

func location(dataDir, channelID string, task *queue.SegmentTask) store.RepLocation {
	return store.RepLocation{
		DataDir: dataDir, ChannelID: channelID,
		PeriodID: task.PeriodID, MediaType: task.MediaType, ASID: task.ASID, RepID: task.RepID,
	}
}

// repDirsHoldFiles reports whether any representation directory of the
// AdaptationSet directory asDir holds a file.
func repDirsHoldFiles(asDir string) bool {
	reps, err := os.ReadDir(asDir)
	if err != nil {
		return false
	}
	for _, rep := range reps {
		if !rep.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(asDir, rep.Name()))
		if err == nil && slices.ContainsFunc(files, func(f os.DirEntry) bool { return !f.IsDir() }) {
			return true
		}
	}
	return false
}

func writeState(asDir string, st asState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := store.WriteDurable(filepath.Join(asDir, StateFileName), data, 0o644); err != nil {
		return fmt.Errorf("write decrypt state: %w", err)
	}
	return nil
}
