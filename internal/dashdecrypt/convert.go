package dashdecrypt

// Converting an AdaptationSet stored as ciphertext to clear, once the channel
// holds every key it needs: the stored files are rewritten beside the live
// directory and the two are swapped with renames, so a reader sees either the
// whole ciphertext AdaptationSet or the whole clear one (ADR-0010).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/a840817a/sluice/internal/cenc"
	"github.com/a840817a/sluice/internal/store"
)

// StagingDirName is the directory under a channel's directory where
// AdaptationSets are converted. It lies outside periods/, so a scan of the
// stored media never sees it, and on the same filesystem, so the swap is a
// rename.
const StagingDirName = ".decrypt-staging"

// Suffixes of the entries a conversion keeps in a staging period directory,
// beside the AdaptationSet directory's own name.
const (
	stagedSuffix = ".new"   // the converted AdaptationSet
	oldSuffix    = ".old"   // the ciphertext one, between the two renames
	readySuffix  = ".ready" // marker: the staged one is complete and durable
)

// Conversion is one stored ciphertext AdaptationSet the registry can convert
// to clear with the keys it holds.
type Conversion struct {
	AS ASKey
	// dir is the live AdaptationSet directory.
	dir string
	// repIDs is every representation with a stored init, and reps holds
	// the protection of each encrypted one.
	repIDs []string
	reps   map[string]cenc.TrackProtection
}

// errSwapStuck: the swap failed after the live directory was moved aside,
// and moving it back failed too.
var errSwapStuck = errors.New("AdaptationSet directory swap stuck halfway")

// rename is os.Rename; tests replace it to make a swap fail.
var rename = os.Rename

// Exclusive runs fn while nothing else writes to the channel's stored media,
// and returns fn's error, or its own when it cannot run fn.
type Exclusive func(ctx context.Context, fn func() error) error

// Wake delivers a value when an AdaptationSet may have become convertible:
// keys were added, or an init of a kept AdaptationSet carried only held KIDs.
func (r *Registry) Wake() <-chan struct{} { return r.wake }

// nudge makes Wake deliver, unless a wake-up is already pending.
func (r *Registry) nudge() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Convertible lists the stored AdaptationSets without a decrypt.json whose
// representations' stored inits all carry a KID the registry holds a key for,
// in a scheme internal/cenc decrypts. An AdaptationSet whose conversion
// failed is left out until a key that may change that is added (AddKeys).
func (r *Registry) Convertible() ([]Conversion, error) {
	periods, err := store.ScanChannel(r.dataDir, r.channelID)
	if err != nil {
		return nil, err
	}
	var out []Conversion
	for _, p := range periods {
		for _, as := range p.AdaptationSets {
			if len(as.Reps) == 0 {
				continue
			}
			key := ASKey{PeriodID: p.ID, ASID: as.ID}
			r.mu.RLock()
			_, decrypted := r.decrypted[key]
			_, failed := r.failed[key]
			r.mu.RUnlock()
			dir := as.Reps[0].ASDir()
			if decrypted || failed {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, StateFileName)); err == nil {
				continue // decided already, but unreadable: Load warned
			}
			var inits []repInit
			var repIDs []string
			for _, rep := range as.Reps {
				if data, err := os.ReadFile(rep.InitPath()); err == nil {
					repIDs = append(repIDs, rep.RepID)
					inits = append(inits, repInit{repID: rep.RepID, data: data})
				}
			}
			r.mu.RLock()
			d := decide(inits, r.keys)
			r.mu.RUnlock()
			if d.warning != nil || len(d.reps) == 0 {
				continue
			}
			out = append(out, Conversion{AS: key, dir: dir, repIDs: repIDs, reps: d.reps})
		}
	}
	return out, nil
}

// Convert rewrites c's stored files as clear ones and swaps them in.
//
// The bulk of the work happens without holding anything: every init is
// cleared and every media segment decrypted into a staging directory, other
// files are copied, and decrypt.json is written. exclusive then runs the
// final phase, in which nothing else writes to the AdaptationSet: what
// arrived since the bulk phase is converted too and what expired is
// dropped, a durable marker is written, and two renames swap the staged
// directory in. RecoverStaging finishes or undoes a swap a crash
// interrupted. Only then does the registry treat the AdaptationSet as
// decrypted, with a new generation and URL version.
//
// A failure leaves the AdaptationSet stored as it was, records a warning
// and removes the staging. A stored segment whose samples rotate to KIDs
// without a key fails it with a missing_key warning naming them, and it is
// tried again once a key for one of them is added; any other failure is a
// conversion_failed warning, tried again once any key is added. When ctx
// ends first the conversion is dropped without a warning: the next start
// tries again.
func (r *Registry) Convert(ctx context.Context, c Conversion, exclusive Exclusive) error {
	paths := stagingPaths(r.dataDir, r.channelID, c.dir)
	err := r.convert(ctx, c, paths, exclusive)
	if err == nil {
		return nil
	}
	// A swap stuck halfway leaves the staged copy, the ciphertext moved
	// aside and the marker for RecoverStaging; removing any of them could
	// lose the AdaptationSet.
	if !errors.Is(err, errSwapStuck) {
		_ = os.RemoveAll(paths.staged)
		_ = os.Remove(paths.ready)
		paths.removeEmptyDirs()
	}
	if ctx.Err() != nil {
		return err
	}
	slog.Warn("AdaptationSet conversion failed", "channel", r.channelID,
		"period", c.AS.PeriodID, "as", c.AS.ASID, "err", err)
	r.mu.Lock()
	defer r.mu.Unlock()
	if mk := (*cenc.MissingKeyError)(nil); errors.As(err, &mk) {
		r.failed[c.AS] = slices.Clone(mk.KIDs)
		r.markSeen(mk.KIDs)
		delete(r.warnings, warningKey{as: c.AS, kind: KindConversionFailed})
		r.recordLocked(c.AS, Warning{Kind: KindMissingKey, KIDs: kidStrings(mk.KIDs),
			Message: "no content key held for these KIDs, which stored segments use, so the AdaptationSet stays encrypted"})
		return err
	}
	r.failed[c.AS] = nil
	r.recordLocked(c.AS, Warning{Kind: KindConversionFailed,
		Message: fmt.Sprintf("stored AdaptationSet not converted to clear, so it stays encrypted: %v", err)})
	return err
}

// Refuse records that c is left stored as ciphertext for reason, in a
// conversion_failed warning, and leaves it out of Convertible until keys are
// added again.
func (r *Registry) Refuse(c Conversion, reason string) {
	r.mu.Lock()
	r.failed[c.AS] = nil
	r.recordLocked(c.AS, Warning{Kind: KindConversionFailed,
		Message: "stored AdaptationSet not converted to clear, so it stays encrypted: " + reason})
	r.mu.Unlock()
}

func (r *Registry) convert(ctx context.Context, c Conversion, paths staging, exclusive Exclusive) error {
	conv := &converter{ctx: ctx, registry: r, src: c.dir, dst: paths.staged, known: make(map[string]bool),
		reps: make(map[string]cenc.TrackProtection), stamps: make(map[string]fileStamp)}
	r.mu.RLock()
	for _, repID := range c.repIDs {
		conv.known[repID] = true
	}
	for repID, tp := range c.reps {
		conv.reps[repID] = tp
	}
	r.mu.RUnlock()

	if err := os.RemoveAll(paths.staged); err != nil {
		return err
	}
	if err := conv.sync(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	return exclusive(ctx, func() error {
		// Taken only once exclusive access is granted: the processor's
		// goroutine, which grants it, waits for init preparations, and those
		// wait for these locks. Nothing holding them waits for the processor.
		defer r.excludeInits(c.AS)()
		defer r.ASLock(c.AS)()
		if err := conv.sync(); err != nil {
			return err
		}
		st := asState{Reps: conv.reps, URLVersion: 1}
		if err := writeState(paths.staged, st); err != nil {
			return err
		}
		if err := conv.syncDirs(); err != nil {
			return err
		}
		if err := store.WriteDurable(paths.ready, nil, 0o644); err != nil {
			return fmt.Errorf("write conversion marker: %w", err)
		}
		if err := rename(c.dir, paths.old); err != nil {
			return err
		}
		if err := rename(paths.staged, c.dir); err != nil {
			// Put the ciphertext back; RecoverStaging does so after a crash.
			if back := rename(paths.old, c.dir); back != nil {
				return fmt.Errorf("%w: %w", errSwapStuck, errors.Join(err, back))
			}
			return err
		}
		// The clear directory is live from here on, so the registry must
		// say so even if the renames cannot be made durable: a crash then
		// leaves the marker for RecoverStaging.
		r.markConverted(c.AS, st)
		if err := errors.Join(store.SyncDir(filepath.Dir(c.dir)), store.SyncDir(filepath.Dir(paths.old))); err != nil {
			slog.Warn("converted AdaptationSet swapped in, but not durably", "channel", r.channelID,
				"period", c.AS.PeriodID, "as", c.AS.ASID, "err", err)
			return nil
		}
		_ = os.Remove(paths.ready)
		_ = os.RemoveAll(paths.old)
		paths.removeEmptyDirs()
		slog.Info("converted stored AdaptationSet to clear", "channel", r.channelID,
			"period", c.AS.PeriodID, "as", c.AS.ASID, "files", len(conv.stamps))
		return nil
	})
}

// markConverted records as decrypted with st: it no longer counts as kept,
// its warnings are resolved, and views of the decrypted set are rebuilt.
func (r *Registry) markConverted(as ASKey, st asState) {
	r.mu.Lock()
	r.decrypted[as] = st
	delete(r.kept, as)
	delete(r.failed, as)
	for k := range r.warnings {
		if k.as == as {
			delete(r.warnings, k)
		}
	}
	r.mu.Unlock()
	r.generation.Add(1)
}

// URLVersion is the version the gateway's URLs of representation repID of
// AdaptationSet asID of period periodID carry, or 0 for none: a representation
// whose stored files a conversion rewrote is served under new URLs, so no
// cache serves its ciphertext again. Two AdaptationSets of a period may use
// the same representation ID; each has its own version.
func (r *Registry) URLVersion(periodID, asID, repID string) uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.decrypted[ASKey{PeriodID: periodID, ASID: asID}]
	if !ok {
		return 0
	}
	if _, ok := st.Reps[repID]; !ok {
		return 0
	}
	return st.URLVersion
}

// staging names the staging entries of one AdaptationSet directory.
type staging struct {
	staged, old, ready string
}

func stagingPaths(dataDir, channelID, asDir string) staging {
	name := filepath.Base(asDir)
	dir := filepath.Join(store.ChannelDir(dataDir, channelID), StagingDirName, filepath.Base(filepath.Dir(asDir)))
	return staging{
		staged: filepath.Join(dir, name+stagedSuffix),
		old:    filepath.Join(dir, name+oldSuffix),
		ready:  filepath.Join(dir, name+readySuffix),
	}
}

// removeEmptyDirs removes the staging directory and its period directory if
// nothing is left in them.
func (s staging) removeEmptyDirs() {
	period := filepath.Dir(s.staged)
	root := filepath.Dir(period)
	removeEmpty(period, root)
	removeEmpty(root, filepath.Dir(root))
}

// fileStamp is what says a stored file changed since it was converted.
type fileStamp struct {
	size, modTime int64
}

// converter mirrors the AdaptationSet directory src into dst, converted.
type converter struct {
	// ctx ends the bulk phase early when the channel stops.
	ctx      context.Context
	registry *Registry
	src, dst string
	// known is every representation whose files may be converted; reps
	// holds the encrypted ones. Their keys come from the registry.
	known map[string]bool
	reps  map[string]cenc.TrackProtection
	// stamps holds each file converted so far, by path relative to src, as
	// it was before it was read.
	stamps map[string]fileStamp
}

// sync brings dst up to date with src: a file new or changed since it was
// last converted is converted again, and one src no longer holds is
// removed, as is a representation directory src no longer has.
func (c *converter) sync() error {
	live := make(map[string]fileStamp)
	dirs := map[string]bool{".": true}
	// Until the final phase, ingest goes on writing and expiring files, so a
	// file listed can be gone by the time it is read; it is then skipped.
	err := filepath.WalkDir(c.src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path != c.src {
				return nil
			}
			return err
		}
		if err := c.ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(c.src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs[rel] = true
			return os.MkdirAll(filepath.Join(c.dst, rel), 0o755)
		}
		if strings.HasPrefix(d.Name(), ".tmp-") {
			return nil // a write in progress; the final phase sees its result
		}
		info, err := d.Info()
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		stamp := fileStamp{size: info.Size(), modTime: info.ModTime().UnixNano()}
		if prev, ok := c.stamps[rel]; ok && prev == stamp {
			live[rel] = stamp
			return nil
		}
		err = c.convertFile(rel)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		live[rel] = stamp
		c.stamps[rel] = stamp
		return nil
	})
	if err != nil {
		return err
	}
	for rel := range c.stamps {
		if _, ok := live[rel]; !ok {
			if err := os.Remove(filepath.Join(c.dst, rel)); err != nil && !os.IsNotExist(err) {
				return err
			}
			delete(c.stamps, rel)
		}
	}
	entries, err := os.ReadDir(c.dst)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && !dirs[e.Name()] {
			if err := os.RemoveAll(filepath.Join(c.dst, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// convertFile writes the converted form of src's file rel to dst: an init
// cleared, a media segment of an encrypted representation decrypted, and
// anything else as it is.
func (c *converter) convertFile(rel string) error {
	data, err := os.ReadFile(filepath.Join(c.src, rel))
	if err != nil {
		return err
	}
	repID, name := "", filepath.Base(rel)
	if dir := filepath.Dir(rel); dir != "." {
		if repID, err = c.repOf(dir); err != nil {
			return err
		}
	}
	tp, encrypted := c.reps[repID]
	switch _, ext, isSeg := store.ParseSegmentFile(name); {
	case !encrypted:
	case name == store.InitFileName:
		if data, err = cenc.ClearInit(data); err != nil {
			return err
		}
	case isSeg && ext == store.ExtFMP4:
		if err := cenc.DecryptSegment(data, tp, c.registry.key); err != nil {
			return err
		}
	}
	return store.Write(filepath.Join(c.dst, rel), data)
}

// repOf returns the ID of the representation in directory dir. One the
// conversion did not start with joins it if its stored init is clear or
// carries a KID the registry holds a key for, and fails it otherwise, so the
// converted AdaptationSet never mixes clear and ciphertext.
func (c *converter) repOf(dir string) (string, error) {
	repID := store.IDFromDirName(dir)
	if c.known[repID] {
		return repID, nil
	}
	data, err := os.ReadFile(filepath.Join(c.src, dir, store.InitFileName))
	if err != nil {
		return "", fmt.Errorf("representation %s arrived without an init: %w", repID, err)
	}
	c.registry.mu.RLock()
	d := decide([]repInit{{repID: repID, data: data}}, c.registry.keys)
	c.registry.mu.RUnlock()
	if d.warning != nil {
		return "", fmt.Errorf("representation %s cannot be decrypted: %s", repID, d.warning.Message)
	}
	if tp, ok := d.reps[repID]; ok {
		c.reps[repID] = tp
	}
	c.known[repID] = true
	return repID, nil
}

// syncDirs makes the entries of every directory of dst durable.
func (c *converter) syncDirs() error {
	return filepath.WalkDir(c.dst, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return store.SyncDir(path)
		}
		return nil
	})
}

// RecoverStaging finishes or undoes the conversions a crash interrupted in
// the channel directory channelDir, and removes what they left in staging.
// It runs before anything reads the channel's stored media.
//
// A conversion whose marker was written had a complete staged directory:
// if the crash came between the two renames, the staged directory becomes
// the live one. Without a marker, or with the live directory in place, the
// staged directory is discarded; the AdaptationSet is then either already
// swapped (it has a decrypt.json) or still ciphertext, to be converted
// again. A ciphertext directory moved aside is put back if no live one
// exists, and deleted only when the live one is the swapped-in copy;
// otherwise recovery fails, keeping both, for an operator to resolve.
func RecoverStaging(channelDir string) error {
	root := filepath.Join(channelDir, StagingDirName)
	periods, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var errs []error
	for _, pe := range periods {
		if !pe.IsDir() {
			continue
		}
		dir := filepath.Join(root, pe.Name())
		liveParent := filepath.Join(channelDir, "periods", pe.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		names := make(map[string]bool)
		for _, e := range entries {
			for _, suffix := range []string{stagedSuffix, oldSuffix, readySuffix} {
				if name, ok := strings.CutSuffix(e.Name(), suffix); ok {
					names[name] = true
				}
			}
		}
		for name := range names {
			errs = append(errs, recoverAS(filepath.Join(liveParent, name), staging{
				staged: filepath.Join(dir, name+stagedSuffix),
				old:    filepath.Join(dir, name+oldSuffix),
				ready:  filepath.Join(dir, name+readySuffix),
			}))
		}
		removeEmpty(dir, root)
	}
	removeEmpty(root, channelDir)
	return errors.Join(errs...)
}

func recoverAS(live string, s staging) error {
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	switch {
	case exists(live):
	case exists(s.ready) && exists(s.staged):
		if err := os.Rename(s.staged, live); err != nil {
			return err
		}
		slog.Info("conversion recovered: staged AdaptationSet swapped in", "dir", live)
	case exists(s.old):
		if err := os.Rename(s.old, live); err != nil {
			return err
		}
		slog.Info("conversion recovered: ciphertext AdaptationSet put back", "dir", live)
	}
	if exists(s.old) && !swappedIn(live) {
		return fmt.Errorf("%s and the copy an interrupted conversion moved aside, %s, both exist: "+
			"keep the complete one as %s and remove the staging directory", live, s.old, live)
	}
	if exists(live) {
		if err := store.SyncDir(filepath.Dir(live)); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(s.old); err != nil {
		return err
	}
	if err := os.RemoveAll(s.staged); err != nil {
		return err
	}
	if err := os.Remove(s.ready); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// swappedIn reports whether the AdaptationSet directory dir is one a
// conversion swapped in: its decrypt.json names a URL version.
func swappedIn(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, StateFileName))
	if err != nil {
		return false
	}
	var st asState
	return json.Unmarshal(raw, &st) == nil && st.URLVersion > 0
}

// removeEmpty removes dir if it is empty, and does nothing to stop.
func removeEmpty(dir, stop string) {
	if dir == stop {
		return
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		_ = os.Remove(dir)
	}
}
