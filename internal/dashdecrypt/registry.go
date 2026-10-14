// Package dashdecrypt decides which AdaptationSets of a DASH channel in key
// mode decrypt are decrypted during ingest, and remembers the decision
// (ADR-0010).
//
// The unit is one AdaptationSet of one period. It is decrypted only when the
// channel holds a key for the KID in the tenc box of every one of its
// representations' init segments, and for every KID their seig sample groups
// name, and the scheme is one internal/cenc decrypts; otherwise every representation of it stays ciphertext, as a
// passthrough channel stores it, and the reason is reported as a Warning.
//
// The decision is stored in the AdaptationSet's directory as decrypt.json,
// which holds each encrypted representation's cenc.TrackProtection, the
// representations that joined later and were refused, and never a key. The file exists exactly when the AdaptationSet is decrypted: it is
// written before the first clear init, or, for an AdaptationSet first stored
// as ciphertext, swapped in with the files Convert rewrote once its keys
// arrived. It is read back when the channel starts.
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
	// failed holds the AdaptationSets whose conversion failed since keys
	// were last added; Convertible leaves them out.
	failed map[ASKey]bool
	// withheld holds, per representation of a decrypted AdaptationSet, the
	// KIDs its segments rotated to without a key held, so they were dropped
	// unstored (RecordSegmentFailure).
	withheld map[RefusedRep]map[cenc.KID]bool
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
		failed:    make(map[ASKey]bool),
		withheld:  make(map[RefusedRep]map[cenc.KID]bool),
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
// (Convertible), including one whose conversion failed before; Wake says
// so.
func (r *Registry) AddKeys(keys []cenc.Key) {
	r.mu.Lock()
	for _, k := range keys {
		r.keys[k.KID] = k.Value
	}
	clear(r.failed)
	r.mu.Unlock()
	r.keyEpoch.Add(1)
	r.nudge()
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

// RefusedRep names one representation IsRefused reports.
type RefusedRep struct {
	AS    ASKey
	RepID string
}

// Refused lists every representation IsRefused reports.
func (r *Registry) Refused() []RefusedRep {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []RefusedRep
	for as, st := range r.decrypted {
		for _, id := range st.Refused {
			out = append(out, RefusedRep{AS: as, RepID: id})
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
		r.decrypted[key] = st
		for _, tp := range st.Reps {
			r.markSeen(tp.KIDs())
		}
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
//     KID has a key, since the stored AdaptationSet may be convertible;
//   - a new one gets every representation's init (fetch, for those of
//     task.ASInits other than task's own) and is decrypted when every KID has
//     a key and every scheme is supported: decrypt.json is written before the
//     clear init is returned. Otherwise it is kept and a warning recorded.
func (r *Registry) PrepareInit(ctx context.Context, task *queue.SegmentTask, orig []byte, held bool, fetch FetchInit) ([]byte, error) {
	key := ASKey{PeriodID: task.PeriodID, ASID: task.ASID}
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
		if d.warning == nil && len(d.reps) > 0 {
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
// can is accepted, which ends an earlier refusal and its missing_key
// warning. Caller holds key's AS lock.
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
		// The warning is recorded last, so whoever sees it also sees the
		// representation hidden.
		defer r.record(key, *d.warning)
		refusal := fmt.Errorf("representation %s cannot join decrypted AdaptationSet %s: %s", task.RepID, key.ASID, d.warning.Message)
		if wasRefused {
			return refusal
		}
		next := st.clone()
		next.Refused = append(next.Refused, task.RepID)
		if err := writeState(asDir, next); err != nil {
			return errors.Join(refusal, err)
		}
		r.mu.Lock()
		r.decrypted[key] = next
		r.mu.Unlock()
		r.generation.Add(1)
		return refusal
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
	if err := writeState(asDir, next); err != nil {
		return err
	}
	r.mu.Lock()
	r.decrypted[key] = next
	if wasRefused {
		r.generation.Add(1)
		if encrypted {
			for _, kid := range tp.KIDs() {
				r.dropMissingKID(key, kid)
			}
		}
	}
	r.mu.Unlock()
	return nil
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
		Refused: slices.Clone(st.Refused)}
	for id, p := range st.Reps {
		out.Reps[id] = p
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

// RecordSegmentFailure reports that a media segment of a decrypted
// AdaptationSet could not be decrypted, so it is not stored. A segment whose
// samples rotate to KIDs the registry holds no key for adds them to the
// AdaptationSet's missing_key warning, and the representation is listed by
// Withheld until a segment decrypts with each of those keys
// (SegmentDecrypted).
func (r *Registry) RecordSegmentFailure(as ASKey, repID string, segNo uint64, err error) {
	if mk := (*cenc.MissingKeyError)(nil); errors.As(err, &mk) {
		r.mu.Lock()
		defer r.mu.Unlock()
		rep := RefusedRep{AS: as, RepID: repID}
		if r.withheld[rep] == nil {
			r.withheld[rep] = make(map[cenc.KID]bool)
		}
		wk := warningKey{as: as, kind: KindMissingKey}
		w, ok := r.warnings[wk]
		if !ok {
			w = Warning{Period: as.PeriodID, AdaptationSet: as.ASID, Kind: KindMissingKey}
		}
		w.KIDs = slices.Clone(w.KIDs)
		for _, kid := range mk.KIDs {
			r.withheld[rep][kid] = true
			r.seen[kid] = true
			if !slices.Contains(w.KIDs, kid.String()) {
				w.KIDs = append(w.KIDs, kid.String())
			}
		}
		slices.Sort(w.KIDs)
		w.Message = fmt.Sprintf("no content key held for these KIDs; segment %d of representation %s and others that use them are withheld", segNo, repID)
		r.warnings[wk] = w
		return
	}
	kind := KindMalformed
	if errors.Is(err, cenc.ErrUnsupportedScheme) {
		kind = KindUnsupportedScheme
	}
	r.record(as, Warning{Kind: kind,
		Message: fmt.Sprintf("segment %d of representation %s not decrypted: %v", segNo, repID, err)})
}

// SegmentDecrypted reports that a segment of as was decrypted with the keys
// of kids, which a fragment's own seig sample group can name though no init
// does: they count as used (key_unmatched). A KID whose segments were
// withheld for want of its key leaves the missing_key warning and Withheld.
func (r *Registry) SegmentDecrypted(as ASKey, kids []cenc.KID) {
	r.mu.RLock()
	pending := slices.ContainsFunc(kids, func(k cenc.KID) bool { return !r.seen[k] })
	for rep, set := range r.withheld {
		if rep.AS == as && slices.ContainsFunc(kids, func(k cenc.KID) bool { return set[k] }) {
			pending = true
			break
		}
	}
	r.mu.RUnlock()
	if !pending {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.markSeen(kids)
	for rep, set := range r.withheld {
		if rep.AS != as {
			continue
		}
		for _, kid := range kids {
			if set[kid] {
				delete(set, kid)
				r.dropMissingKID(as, kid)
			}
		}
		if len(set) == 0 {
			delete(r.withheld, rep)
		}
	}
}

// Withheld lists the representations whose segments were dropped for want
// of a key and have not decrypted with it since. Once its key is added, a
// live origin's next poll should offer those segments again.
func (r *Registry) Withheld() []RefusedRep {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]RefusedRep, 0, len(r.withheld))
	for rep := range r.withheld {
		out = append(out, rep)
	}
	return out
}

func (r *Registry) record(as ASKey, w Warning) {
	w.Period, w.AdaptationSet = as.PeriodID, as.ASID
	r.mu.Lock()
	r.warnings[warningKey{as: as, kind: w.Kind}] = w
	r.mu.Unlock()
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
