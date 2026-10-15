package dashdecrypt

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/cenc"
	"github.com/a840817a/sluice/internal/store"
)

// audioLoc is where the tests store representation a0 of the audio
// AdaptationSet "1" of period p0.
func audioLoc(dataDir string) store.RepLocation {
	return store.RepLocation{DataDir: dataDir, ChannelID: "c", PeriodID: "p0", MediaType: "audio", ASID: "1", RepID: "a0"}
}

var audioFixture = []string{"audio", "und", "mp4a.40.2"}

func audioFile(t *testing.T, variant, file string) []byte {
	t.Helper()
	return readFixture(t, append(append([]string{variant}, audioFixture...), file)...)
}

// storeCiphertextAudio stores the cenc audio init and segments as a
// passthrough channel would, with a sidecar beside them.
func storeCiphertextAudio(t *testing.T, dataDir string, segs ...int) store.RepLocation {
	t.Helper()
	loc := audioLoc(dataDir)
	if err := store.Write(loc.InitPath(), audioFile(t, "cenc", "init.mp4")); err != nil {
		t.Fatal(err)
	}
	for _, n := range segs {
		if err := store.Write(loc.SegmentPath(uint64(n), store.ExtFMP4), audioFile(t, "cenc", fmt.Sprintf("seg-%d.m4s", n))); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Write(loc.SidecarPath(), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	return loc
}

// mdat returns the payload of seg's top-level mdat box.
func mdat(t *testing.T, seg []byte) []byte {
	t.Helper()
	for off := 0; off+8 <= len(seg); {
		size := int(binary.BigEndian.Uint32(seg[off:]))
		if size < 8 || off+size > len(seg) {
			break
		}
		if string(seg[off+4:off+8]) == "mdat" {
			return seg[off+8 : off+size]
		}
		off += size
	}
	t.Fatalf("segment of %d bytes has no mdat", len(seg))
	return nil
}

// loadedRegistry is a registry over dataDir that has read what is stored,
// holding keys.
func loadedRegistry(t *testing.T, dataDir string, keys ...string) *Registry {
	t.Helper()
	r := NewRegistry(dataDir, "c", keySet(t, keys...))
	if err := r.Load(); err != nil {
		t.Fatal(err)
	}
	return r
}

func onlyConversion(t *testing.T, r *Registry) Conversion {
	t.Helper()
	pending, err := r.Convertible()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].AS != (ASKey{"p0", "1"}) {
		t.Fatalf("Convertible = %+v, want the audio AdaptationSet", pending)
	}
	return pending[0]
}

func run(_ context.Context, fn func() error) error { return fn() }

// A conversion rewrites the stored AdaptationSet as clear, including what
// ingest stored while the bulk of it ran, and leaves out what ingest expired
// meanwhile. The registry then treats it as decrypted.
func TestConvertCatchesUpBeforeSwap(t *testing.T) {
	dataDir := t.TempDir()
	loc := storeCiphertextAudio(t, dataDir, 1, 2, 3)
	r := loadedRegistry(t, dataDir, audioKey)
	if ws := r.Warnings(); len(ws) != 0 {
		t.Fatalf("warnings before conversion = %+v, want none", ws)
	}
	c := onlyConversion(t, r)
	gen := r.Generation()

	// Between the bulk and the final phase, ingest stores segment 4 and
	// expires segment 2.
	err := r.Convert(context.Background(), c, func(ctx context.Context, fn func() error) error {
		if err := store.Write(loc.SegmentPath(4, store.ExtFMP4), audioFile(t, "cenc", "seg-4.m4s")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(loc.SegmentPath(2, store.ExtFMP4)); err != nil {
			t.Fatal(err)
		}
		return fn()
	})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	init, err := os.ReadFile(loc.InitPath())
	if err != nil || !isClear(t, init) {
		t.Errorf("init after conversion: err %v, want clear", err)
	}
	for _, n := range []int{1, 3, 4} {
		got, err := os.ReadFile(loc.SegmentPath(uint64(n), store.ExtFMP4))
		if err != nil {
			t.Errorf("segment %d: %v", n, err)
			continue
		}
		if !bytes.Equal(mdat(t, got), mdat(t, audioFile(t, "clear", fmt.Sprintf("seg-%d.m4s", n)))) {
			t.Errorf("segment %d: samples differ from the clear fixture's", n)
		}
	}
	if _, err := os.Stat(loc.SegmentPath(2, store.ExtFMP4)); !os.IsNotExist(err) {
		t.Errorf("segment 2, expired during the conversion, came back: %v", err)
	}
	if sidecar, err := os.ReadFile(loc.SidecarPath()); err != nil || string(sidecar) != "{}\n" {
		t.Errorf("sidecar = %q, %v; want it copied as it was", sidecar, err)
	}
	raw, err := os.ReadFile(filepath.Join(loc.ASDir(), StateFileName))
	if err != nil {
		t.Fatalf("no %s: %v", StateFileName, err)
	}
	var st asState
	if err := json.Unmarshal(raw, &st); err != nil || st.URLVersion != 1 || st.Reps["a0"].KID.String() != audioKID {
		t.Errorf("%s = %s (%v), want a0's protection and url_version 1", StateFileName, raw, err)
	}
	if _, err := os.Stat(filepath.Join(store.ChannelDir(dataDir, "c"), StagingDirName)); !os.IsNotExist(err) {
		t.Errorf("staging left behind: %v", err)
	}

	if !r.IsDecrypted(c.AS) {
		t.Error("registry does not treat the converted AdaptationSet as decrypted")
	}
	if r.Generation() == gen {
		t.Error("conversion did not change the generation")
	}
	if v := r.URLVersion("p0", "1", "a0"); v != 1 {
		t.Errorf("URLVersion = %d, want 1", v)
	}
	if _, _, ok := r.Lookup(c.AS, "a0"); !ok {
		t.Error("Lookup does not decrypt segments ingested after the conversion")
	}
	if pending, _ := r.Convertible(); len(pending) != 0 {
		t.Errorf("Convertible after conversion = %+v, want none", pending)
	}

	// The URL version survives a restart.
	if v := loadedRegistry(t, dataDir, audioKey).URLVersion("p0", "1", "a0"); v != 1 {
		t.Errorf("URLVersion after Load = %d, want 1", v)
	}
}

// An AdaptationSet without a key for every KID is not convertible, and
// becomes so when the key is added.
func TestConvertibleNeedsEveryKey(t *testing.T) {
	dataDir := t.TempDir()
	storeCiphertextAudio(t, dataDir, 1)
	r := loadedRegistry(t, dataDir, videoKey)
	if pending, _ := r.Convertible(); len(pending) != 0 {
		t.Fatalf("Convertible without the audio key = %+v", pending)
	}
	r.AddKeys([]cenc.Key{mustKey(t, audioKey)})
	select {
	case <-r.Wake():
	default:
		t.Error("AddKeys did not wake the converter")
	}
	onlyConversion(t, r)
}

// A conversion that fails leaves the AdaptationSet exactly as stored,
// removes its staging and says why; it is not retried until keys are added.
func TestConvertFailureKeepsCiphertext(t *testing.T) {
	dataDir := t.TempDir()
	loc := storeCiphertextAudio(t, dataDir, 1, 2)
	corrupt := audioFile(t, "cenc", "seg-2.m4s")[:100]
	if err := store.Write(loc.SegmentPath(2, store.ExtFMP4), corrupt); err != nil {
		t.Fatal(err)
	}
	r := loadedRegistry(t, dataDir, audioKey)
	c := onlyConversion(t, r)

	if err := r.Convert(context.Background(), c, run); err == nil {
		t.Fatal("Convert of a corrupt segment succeeded")
	}
	if got, _ := os.ReadFile(loc.SegmentPath(1, store.ExtFMP4)); !bytes.Equal(got, audioFile(t, "cenc", "seg-1.m4s")) {
		t.Error("segment 1 changed by a failed conversion")
	}
	if got, _ := os.ReadFile(loc.SegmentPath(2, store.ExtFMP4)); !bytes.Equal(got, corrupt) {
		t.Error("segment 2 changed by a failed conversion")
	}
	if got, _ := os.ReadFile(loc.InitPath()); !bytes.Equal(got, audioFile(t, "cenc", "init.mp4")) {
		t.Error("init changed by a failed conversion")
	}
	if _, err := os.Stat(filepath.Join(loc.ASDir(), StateFileName)); !os.IsNotExist(err) {
		t.Errorf("failed conversion left %s: %v", StateFileName, err)
	}
	if _, err := os.Stat(filepath.Join(store.ChannelDir(dataDir, "c"), StagingDirName)); !os.IsNotExist(err) {
		t.Errorf("staging left behind: %v", err)
	}
	if r.IsDecrypted(c.AS) {
		t.Error("AdaptationSet decrypted after a failed conversion")
	}
	ws := r.Warnings()
	if len(ws) != 1 || ws[0].Kind != KindConversionFailed || ws[0].AdaptationSet != "1" {
		t.Errorf("warnings = %+v, want conversion_failed for the audio AdaptationSet", ws)
	}
	for _, w := range ws {
		if bytes.Contains([]byte(w.Message), []byte("ffeeddccbbaa99887766554433221100")) {
			t.Errorf("warning holds a key value: %+v", w)
		}
	}
	if pending, _ := r.Convertible(); len(pending) != 0 {
		t.Errorf("a failed AdaptationSet is convertible again before keys are added: %+v", pending)
	}
	r.AddKeys([]cenc.Key{mustKey(t, "30303030303030303030303030303030:0123456789abcdef0123456789abcdef")})
	onlyConversion(t, r)
}

// A conversion that meets a stored segment rotating to a KID without a key
// leaves the AdaptationSet as stored and reports missing_key for that KID,
// not conversion_failed. Only adding that KID's key makes it convertible
// again.
func TestConvertMissingRotationKey(t *testing.T) {
	dataDir := t.TempDir()
	loc := storeCiphertextAudio(t, dataDir, 1)
	audio, missing := mustKey(t, audioKey), mustKey(t, missingKey)
	seg2 := rotated(t, audioFixture, "seg-2.m4s", audio, missing)
	if err := store.Write(loc.SegmentPath(2, store.ExtFMP4), seg2); err != nil {
		t.Fatal(err)
	}
	r := loadedRegistry(t, dataDir, audioKey)
	c := onlyConversion(t, r)

	if err := r.Convert(context.Background(), c, run); err == nil {
		t.Fatal("Convert of a segment without its key succeeded")
	}
	if got, _ := os.ReadFile(loc.SegmentPath(2, store.ExtFMP4)); !bytes.Equal(got, seg2) {
		t.Error("segment 2 changed by a failed conversion")
	}
	if got, _ := os.ReadFile(loc.InitPath()); !bytes.Equal(got, audioFile(t, "cenc", "init.mp4")) {
		t.Error("init changed by a failed conversion")
	}
	if r.IsDecrypted(c.AS) {
		t.Error("AdaptationSet decrypted after a failed conversion")
	}
	ws := r.Warnings()
	if len(ws) != 1 || ws[0].Kind != KindMissingKey || ws[0].AdaptationSet != "1" || len(ws[0].KIDs) != 1 || ws[0].KIDs[0] != missing.KID.String() {
		t.Errorf("warnings = %+v, want only missing_key for %s", ws, missing.KID)
	}

	r.AddKeys([]cenc.Key{mustKey(t, videoKey)})
	if pending, _ := r.Convertible(); len(pending) != 0 {
		t.Errorf("convertible again after an unrelated key: %+v", pending)
	}
	r.AddKeys([]cenc.Key{missing})
	if _, ok := warningOf(r.Warnings(), KindMissingKey); ok {
		t.Errorf("warnings = %+v, want no missing_key once its key is held", r.Warnings())
	}
	if err := r.Convert(context.Background(), onlyConversion(t, r), run); err != nil {
		t.Fatalf("Convert with the key: %v", err)
	}
	got, _ := os.ReadFile(loc.SegmentPath(2, store.ExtFMP4))
	if !bytes.Equal(mdat(t, got), mdat(t, audioFile(t, "clear", "seg-2.m4s"))) {
		t.Error("converted segment 2 differs from the clear fixture's")
	}
	// Only the unrelated key is left to report.
	if ws := r.Warnings(); len(ws) != 1 || ws[0].Kind != KindKeyUnmatched {
		t.Errorf("warnings after the conversion = %+v, want only key_unmatched", ws)
	}
}

// A conversion dropped because the channel is stopping leaves no warning.
func TestConvertDroppedWhenStopping(t *testing.T) {
	dataDir := t.TempDir()
	storeCiphertextAudio(t, dataDir, 1)
	r := loadedRegistry(t, dataDir, audioKey)
	c := onlyConversion(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	err := r.Convert(ctx, c, func(context.Context, func() error) error {
		cancel()
		return context.Canceled
	})
	if err == nil {
		t.Fatal("Convert succeeded without running the final phase")
	}
	if ws := r.Warnings(); len(ws) != 0 {
		t.Errorf("warnings = %+v, want none for a dropped conversion", ws)
	}
	onlyConversion(t, r)
}

// RecoverStaging leaves every AdaptationSet a crash interrupted whole: the
// converted one where the marker says it was complete and nothing else is
// live, the ciphertext one otherwise; and it removes the staging.
func TestRecoverStaging(t *testing.T) {
	const (
		live   = "live"
		staged = "new"
		old    = "old"
	)
	cases := []struct {
		name string
		// what exists before recovery: the live directory's content ("" for
		// none), the staged and moved-aside ones, and the marker.
		live, staged, old string
		ready             bool
		// want is the live directory's content after recovery; wantErr
		// says recovery must refuse, keeping every copy.
		want    string
		wantErr bool
	}{
		{name: "crash between the renames", staged: staged, old: old, ready: true, want: staged},
		{name: "crash before the renames", live: live, staged: staged, ready: true, want: live},
		{name: "crash after the renames", live: staged, old: old, ready: true, want: staged},
		{name: "crash before the marker", live: live, staged: staged, want: live},
		{name: "marker without the staged directory", old: old, ready: true, want: old},
		{name: "nothing staged", live: live, want: live},
		// Ingest recreated the live directory after a swap failed with
		// the ciphertext moved aside: neither copy may go.
		{name: "live recreated after a failed swap", live: live, staged: staged, old: old, ready: true, want: live, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			channelDir := filepath.Join(t.TempDir(), "c")
			asDir := filepath.Join(channelDir, "periods", "p0", "audio_1")
			stagingDir := filepath.Join(channelDir, StagingDirName, "p0")
			write := func(dir, content string) {
				t.Helper()
				if content == "" {
					return
				}
				if err := os.MkdirAll(filepath.Join(dir, "a0"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "a0", "init.mp4"), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
				if content == staged {
					if err := os.WriteFile(filepath.Join(dir, StateFileName), []byte(`{"reps":{},"url_version":1}`), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			write(asDir, tc.live)
			write(filepath.Join(stagingDir, "audio_1"+stagedSuffix), tc.staged)
			write(filepath.Join(stagingDir, "audio_1"+oldSuffix), tc.old)
			if err := os.MkdirAll(filepath.Join(channelDir, "periods", "p0"), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.ready {
				if err := os.MkdirAll(stagingDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(stagingDir, "audio_1"+readySuffix), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			err := RecoverStaging(channelDir)
			got, readErr := os.ReadFile(filepath.Join(asDir, "a0", "init.mp4"))
			if readErr != nil || string(got) != tc.want {
				t.Errorf("live AdaptationSet holds %q (%v), want %q", got, readErr, tc.want)
			}
			if tc.wantErr {
				if err == nil {
					t.Error("RecoverStaging succeeded, want an error")
				}
				for _, p := range []string{"audio_1" + oldSuffix, "audio_1" + stagedSuffix} {
					if _, err := os.Stat(filepath.Join(stagingDir, p)); err != nil {
						t.Errorf("refused recovery removed %s: %v", p, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("RecoverStaging: %v", err)
			}
			if _, err := os.Stat(filepath.Join(channelDir, StagingDirName)); !os.IsNotExist(err) {
				t.Errorf("staging left behind: %v", err)
			}
		})
	}
	if err := RecoverStaging(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Errorf("RecoverStaging of a channel without staging: %v", err)
	}
}

// An init of an AdaptationSet kept as ciphertext whose KID has a key wakes
// the converter: the stored AdaptationSet may be convertible now.
func TestPrepareInitOfKeptAdaptationSetWakes(t *testing.T) {
	dataDir := t.TempDir()
	storeCiphertextAudio(t, dataDir, 1)
	r := loadedRegistry(t, dataDir, videoKey)
	r.AddKeys([]cenc.Key{mustKey(t, audioKey)})
	<-r.Wake()

	audio := audioFile(t, "cenc", "init.mp4")
	next := audioLoc(dataDir)
	task := task("a1")
	task.PeriodID, task.ASID, task.MediaType = next.PeriodID, next.ASID, next.MediaType
	if got, err := r.PrepareInit(context.Background(), task, audio, true, nil); err != nil || !bytes.Equal(got, audio) {
		t.Fatalf("PrepareInit of a kept AdaptationSet: err %v, init changed %v", err, !bytes.Equal(got, audio))
	}
	select {
	case <-r.Wake():
	default:
		t.Error("PrepareInit of a kept AdaptationSet with its key held did not wake the converter")
	}
}

// Convert holds nothing an init preparation needs while it waits for
// exclusive access: the processor's goroutine, which grants it, waits for
// every init preparation before it stops.
func TestConvertHoldsNoLockWhileWaitingForExclusive(t *testing.T) {
	dataDir := t.TempDir()
	storeCiphertextAudio(t, dataDir, 1)
	r := loadedRegistry(t, dataDir, audioKey)
	c := onlyConversion(t, r)
	err := r.Convert(context.Background(), c, func(ctx context.Context, fn func() error) error {
		prepared := make(chan struct{})
		go func() {
			r.HoldInit(c.AS)()
			r.ASLock(c.AS)()
			close(prepared)
		}()
		select {
		case <-prepared:
		case <-time.After(2 * time.Second):
			return errors.New("an init preparation blocked while Convert waited for exclusive access")
		}
		return fn()
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A representation that joins a converted AdaptationSet keeps its URLs
// versioned, like the rest of it.
func TestRepJoinsAfterConversionKeepsURLVersion(t *testing.T) {
	dataDir := t.TempDir()
	storeCiphertextAudio(t, dataDir, 1)
	r := loadedRegistry(t, dataDir, audioKey)
	if err := r.Convert(context.Background(), onlyConversion(t, r), run); err != nil {
		t.Fatal(err)
	}
	loc := audioLoc(dataDir)
	next := task("a1")
	next.PeriodID, next.ASID, next.MediaType = loc.PeriodID, loc.ASID, loc.MediaType
	if _, err := r.PrepareInit(context.Background(), next, audioFile(t, "cenc", "init.mp4"), true, nil); err != nil {
		t.Fatal(err)
	}
	for _, rep := range []string{"a0", "a1"} {
		if v := r.URLVersion("p0", "1", rep); v != 1 {
			t.Errorf("URLVersion of %s = %d, want 1", rep, v)
		}
	}
	if v := loadedRegistry(t, dataDir, audioKey).URLVersion("p0", "1", "a1"); v != 1 {
		t.Errorf("URLVersion of a1 after Load = %d, want 1", v)
	}
}

// A swap that fails and cannot be undone deletes nothing: the ciphertext
// moved aside, the staged clear copy and the marker stay for RecoverStaging,
// which completes the swap.
func TestConvertStuckSwapKeepsBothCopies(t *testing.T) {
	dataDir := t.TempDir()
	loc := storeCiphertextAudio(t, dataDir, 1)
	r := loadedRegistry(t, dataDir, audioKey)
	c := onlyConversion(t, r)
	defer func(f func(string, string) error) { rename = f }(rename)
	rename = func(from, to string) error {
		if to == loc.ASDir() {
			return errors.New("rename refused")
		}
		return os.Rename(from, to)
	}
	if err := r.Convert(context.Background(), c, run); err == nil {
		t.Fatal("Convert succeeded although the swap failed")
	}
	paths := stagingPaths(dataDir, "c", loc.ASDir())
	for _, p := range []string{paths.old, paths.staged, paths.ready} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("failed swap removed %s: %v", filepath.Base(p), err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(paths.old, "a0", "1.m4s")); !bytes.Equal(got, audioFile(t, "cenc", "seg-1.m4s")) {
		t.Error("ciphertext moved aside is gone")
	}

	rename = os.Rename
	if err := RecoverStaging(store.ChannelDir(dataDir, "c")); err != nil {
		t.Fatalf("RecoverStaging: %v", err)
	}
	if _, err := os.Stat(filepath.Join(loc.ASDir(), StateFileName)); err != nil {
		t.Errorf("recovery did not swap the converted AdaptationSet in: %v", err)
	}
}

// Two AdaptationSets of one period may use the same representation ID; a
// version belongs to the AdaptationSet that was converted, not to the ID.
func TestURLVersionIsPerAdaptationSet(t *testing.T) {
	r := NewRegistry(t.TempDir(), "c", nil)
	reps := map[string]cenc.TrackProtection{"x": {}}
	r.decrypted[ASKey{"p0", "1"}] = asState{Reps: reps, URLVersion: 1}
	r.decrypted[ASKey{"p0", "2"}] = asState{Reps: reps}
	if v := r.URLVersion("p0", "1", "x"); v != 1 {
		t.Errorf("converted AdaptationSet: URLVersion = %d, want 1", v)
	}
	if v := r.URLVersion("p0", "2", "x"); v != 0 {
		t.Errorf("unconverted AdaptationSet: URLVersion = %d, want 0", v)
	}
	if v := r.URLVersion("p0", "1", "other"); v != 0 {
		t.Errorf("unknown representation: URLVersion = %d, want 0", v)
	}
}

// A missing_key warning says the same thing whichever part of the channel
// lacks the key: a stored AdaptationSet a conversion could not finish, or
// segments a decrypted one withheld, which it names.
func TestMissingKeyMessageIsShared(t *testing.T) {
	const base = "no content key held for these KIDs"
	dataDir := t.TempDir()
	loc := storeCiphertextAudio(t, dataDir, 1)
	audio, missing := mustKey(t, audioKey), mustKey(t, missingKey)
	if err := store.Write(loc.SegmentPath(2, store.ExtFMP4), rotated(t, audioFixture, "seg-2.m4s", audio, missing)); err != nil {
		t.Fatal(err)
	}
	r := loadedRegistry(t, dataDir, audioKey)
	if err := r.Convert(context.Background(), onlyConversion(t, r), run); err == nil {
		t.Fatal("Convert of a segment without its key succeeded")
	}
	converting, ok := warningOf(r.Warnings(), KindMissingKey)
	if !ok || converting.Message != base {
		t.Errorf("conversion's missing_key = %+v, want message %q", converting, base)
	}

	withholding := decryptedVideo(t, t.TempDir(), videoKey)
	if err := withholding.DecryptSegment(segTask(3), rotated(t, videoRep, "seg-1.m4s", mustKey(t, videoKey), missing)); err == nil {
		t.Fatal("decrypted without the key")
	}
	w, ok := warningOf(withholding.Warnings(), KindMissingKey)
	if want := base + "; segments that use them withheld from representations: v0"; !ok || w.Message != want {
		t.Errorf("decrypted AdaptationSet's missing_key = %+v, want message %q", w, want)
	}
	if !slices.Equal(converting.KIDs, w.KIDs) {
		t.Errorf("KIDs differ for the same missing key: %v and %v", converting.KIDs, w.KIDs)
	}
}
