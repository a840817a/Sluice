// Package dashdecrypt decides which AdaptationSets of a DASH channel in key
// mode decrypt are decrypted during ingest, and remembers the decision
// (ADR-0010).
//
// The unit is one AdaptationSet of one period. It is decrypted only when the
// channel holds a key for the KID in the tenc box of every one of its
// representations' init segments and the scheme is one internal/cenc
// decrypts; otherwise every representation of it stays ciphertext, as a
// passthrough channel stores it, and the reason is reported as a Warning.
//
// The decision is stored in the AdaptationSet's directory as decrypt.json,
// which holds each encrypted representation's cenc.TrackProtection and never
// a key. The file exists exactly when the AdaptationSet is decrypted: it is
// written before the first clear init, and read back when the channel starts.
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
}

// FetchInit fetches the init segment ref locates from the origin.
type FetchInit func(ctx context.Context, ref queue.InitRef) ([]byte, error)

// Registry is one channel's decryption state. It is safe for concurrent use.
type Registry struct {
	dataDir, channelID string
	// generation changes whenever an AdaptationSet becomes decrypted, so a
	// view derived from the decrypted set can be cached against it.
	generation atomic.Uint64

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

	locksMu sync.Mutex
	locks   map[ASKey]*sync.Mutex
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
		locks:     make(map[ASKey]*sync.Mutex),
	}
	for kid, k := range keys {
		r.keys[kid] = k
	}
	return r
}

// AddKeys gives the registry more keys. AdaptationSets decided from now on
// can use them; one already stored as ciphertext stays so.
func (r *Registry) AddKeys(keys []cenc.Key) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range keys {
		r.keys[k.KID] = k.Value
	}
}

// Generation identifies the current set of decrypted AdaptationSets.
func (r *Registry) Generation() uint64 { return r.generation.Load() }

// IsDecrypted reports whether the AdaptationSet as is decrypted.
func (r *Registry) IsDecrypted(as ASKey) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.decrypted[as]
	return ok
}

// Lookup returns how to decrypt the media of representation repID of as, and
// false when it is stored as the origin produced it: as is not decrypted, or
// the representation is clear.
func (r *Registry) Lookup(as ASKey, repID string) (cenc.TrackProtection, [16]byte, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.decrypted[as]
	if !ok {
		return cenc.TrackProtection{}, [16]byte{}, false
	}
	tp, ok := st.Reps[repID]
	if !ok {
		return cenc.TrackProtection{}, [16]byte{}, false
	}
	key, ok := r.keys[tp.KID]
	return tp, key, ok
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
			r.seen[tp.KID] = true
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
//     already decided to keep, gets orig;
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
// did not cover. Caller holds key's AS lock.
func (r *Registry) addRep(key ASKey, task *queue.SegmentTask, st asState, orig []byte) error {
	r.mu.RLock()
	d := decide([]repInit{{repID: task.RepID, data: orig}}, r.keys)
	r.mu.RUnlock()
	r.mu.Lock()
	r.markSeen(d.kids)
	r.mu.Unlock()
	if d.warning != nil {
		r.record(key, *d.warning)
		return fmt.Errorf("representation %s cannot join decrypted AdaptationSet %s: %s", task.RepID, key.ASID, d.warning.Message)
	}
	tp, ok := d.reps[task.RepID]
	if !ok {
		return nil // a clear representation needs no entry
	}
	next := asState{Reps: make(map[string]cenc.TrackProtection, len(st.Reps)+1)}
	for id, p := range st.Reps {
		next.Reps[id] = p
	}
	next.Reps[task.RepID] = tp
	if err := writeState(location(r.dataDir, r.channelID, task).ASDir(), next); err != nil {
		return err
	}
	r.mu.Lock()
	r.decrypted[key] = next
	r.mu.Unlock()
	return nil
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
				r.seen[tp.KID] = true
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
// AdaptationSet could not be decrypted.
func (r *Registry) RecordSegmentFailure(as ASKey, repID string, segNo uint64, err error) {
	kind := KindMalformed
	if errors.Is(err, cenc.ErrUnsupportedScheme) || errors.Is(err, cenc.ErrKeyRotation) {
		kind = KindUnsupportedScheme
	}
	r.record(as, Warning{Kind: kind,
		Message: fmt.Sprintf("segment %d of representation %s not decrypted: %v", segNo, repID, err)})
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
