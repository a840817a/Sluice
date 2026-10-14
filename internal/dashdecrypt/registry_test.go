package dashdecrypt

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/cenc"
	"github.com/a840817a/sluice/internal/cenc/cenctest"
	"github.com/a840817a/sluice/internal/queue"
	"github.com/a840817a/sluice/internal/store"
)

const fixtures = "../../testdata/cenc"

const (
	videoKey = "10101010101010101010101010101010:00112233445566778899aabbccddeeff"
	audioKey = "20202020202020202020202020202020:ffeeddccbbaa99887766554433221100"
	videoKID = "10101010101010101010101010101010"
	audioKID = "20202020202020202020202020202020"
)

func readFixture(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{fixtures}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func keySet(t *testing.T, keys ...string) cenc.KeySet {
	t.Helper()
	ks := cenc.KeySet{}
	for _, s := range keys {
		k, err := cenc.ParseKey(s)
		if err != nil {
			t.Fatal(err)
		}
		ks[k.KID] = k.Value
	}
	return ks
}

// withScheme returns init with the scheme in its schm box replaced.
func withScheme(t *testing.T, init []byte, scheme string) []byte {
	t.Helper()
	i := bytes.Index(init, []byte("schm"))
	if i < 0 {
		t.Fatal("init has no schm box")
	}
	out := slices.Clone(init)
	copy(out[i+8:i+12], scheme)
	return out
}

func task(reps ...string) *queue.SegmentTask {
	t := &queue.SegmentTask{ChannelID: "c", PeriodID: "p0", ASID: "0", RepID: reps[0], MediaType: "video"}
	for _, r := range reps {
		t.ASInits = append(t.ASInits, queue.InitRef{RepID: r, InitURL: "http://origin/" + r})
	}
	return t
}

func isClear(t *testing.T, init []byte) bool {
	t.Helper()
	tps, err := cenc.ReadInitProtection(init)
	if err != nil {
		t.Fatalf("read returned init: %v", err)
	}
	return tps == nil
}

// The decision for a new AdaptationSet: decrypted only when every
// representation is encrypted with a supported scheme under a held key.
func TestPrepareInitDecidesNewAdaptationSet(t *testing.T) {
	defer func(d time.Duration) { siblingBackoff = d }(siblingBackoff)
	siblingBackoff = time.Millisecond
	video := readFixture(t, "cenc", "video", "avc1", "init.mp4")
	audio := readFixture(t, "cenc", "audio", "und", "mp4a.40.2", "init.mp4")
	cbcsVideo := readFixture(t, "cbcs", "video", "avc1", "init.mp4")
	clearVideo := readFixture(t, "clear", "video", "avc1", "init.mp4")
	cens := withScheme(t, video, "cens")
	rotating := rotationInit(t, video, rotKID)

	// The AdaptationSet's representations are "r0" (whose init is being
	// prepared) and, where siblings is set, "r1".
	cases := []struct {
		name      string
		own       []byte
		sibling   []byte // nil: a single-representation AdaptationSet
		keys      []string
		fetchErr  bool
		decrypted bool
		kind      string // the warning recorded, if any
		kids      []string
	}{
		{name: "one rep, key held", own: video, keys: []string{videoKey}, decrypted: true},
		{name: "cbcs, key held", own: cbcsVideo, keys: []string{videoKey}, decrypted: true},
		{name: "two reps, both keys", own: video, sibling: audio, keys: []string{videoKey, audioKey}, decrypted: true},
		{name: "one rep, no key", own: video, keys: []string{audioKey}, kind: KindMissingKey, kids: []string{videoKID}},
		{name: "sibling's key missing", own: video, sibling: audio, keys: []string{videoKey},
			kind: KindMissingKey, kids: []string{audioKID}},
		{name: "own key missing", own: video, sibling: audio, keys: []string{audioKey},
			kind: KindMissingKey, kids: []string{videoKID}},
		{name: "unsupported scheme", own: cens, keys: []string{videoKey}, kind: KindUnsupportedScheme},
		{name: "sibling with unsupported scheme", own: video, sibling: withScheme(t, audio, "cbc1"),
			keys: []string{videoKey, audioKey}, kind: KindUnsupportedScheme},
		{name: "malformed init", own: video[:40], keys: []string{videoKey}, kind: KindMalformed},
		{name: "sibling unavailable", own: video, sibling: audio, fetchErr: true, keys: []string{videoKey, audioKey},
			kind: KindMalformed},
		{name: "clear", own: clearVideo, keys: []string{videoKey}},
		{name: "seig groups, every key", own: rotating, keys: []string{videoKey, rotKey}, decrypted: true},
		{name: "seig group's key missing", own: rotating, keys: []string{videoKey},
			kind: KindMissingKey, kids: []string{rotKIDHex}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			r := NewRegistry(dir, "c", keySet(t, tc.keys...))
			tk := task("r0")
			if tc.sibling != nil {
				tk = task("r0", "r1")
			}
			fetches := 0
			fetch := func(ctx context.Context, ref queue.InitRef) ([]byte, error) {
				fetches++
				if ref.RepID != "r1" {
					t.Errorf("fetched %q, want only the sibling r1", ref.RepID)
				}
				if tc.fetchErr {
					return nil, errors.New("origin down")
				}
				return tc.sibling, nil
			}
			gen := r.Generation()
			got, err := r.PrepareInit(context.Background(), tk, tc.own, false, fetch)
			if err != nil {
				t.Fatalf("PrepareInit: %v", err)
			}
			as := ASKey{PeriodID: "p0", ASID: "0"}
			statePath := filepath.Join(store.RepLocation{DataDir: dir, ChannelID: "c", PeriodID: "p0", MediaType: "video", ASID: "0", RepID: "r0"}.ASDir(), StateFileName)
			_, statErr := os.Stat(statePath)

			if r.IsDecrypted(as) != tc.decrypted || (statErr == nil) != tc.decrypted {
				t.Fatalf("decrypted = %v with state file %v, want %v", r.IsDecrypted(as), statErr == nil, tc.decrypted)
			}
			if tc.decrypted {
				if !isClear(t, got) {
					t.Error("a decrypted AdaptationSet got an encrypted init")
				}
				if r.Generation() == gen {
					t.Error("generation did not change when the AdaptationSet was decrypted")
				}
				if _, _, ok := r.Lookup(as, "r0"); !ok {
					t.Error("Lookup finds no key for the decrypted representation")
				}
				state, _ := os.ReadFile(statePath)
				for _, k := range tc.keys {
					if _, v, _ := strings.Cut(k, ":"); strings.Contains(string(state), v) {
						t.Errorf("%s holds a key value", StateFileName)
					}
				}
			} else {
				if !bytes.Equal(got, tc.own) {
					t.Error("a kept AdaptationSet's init was changed")
				}
				if _, _, ok := r.Lookup(as, "r0"); ok {
					t.Error("Lookup decrypts media of a kept AdaptationSet")
				}
			}
			var kinds []Warning
			for _, w := range r.Warnings() {
				if w.Kind != KindKeyUnmatched {
					kinds = append(kinds, w)
				}
			}
			switch {
			case tc.kind == "" && len(kinds) > 0:
				t.Errorf("warnings = %+v, want none", kinds)
			case tc.kind != "" && (len(kinds) != 1 || kinds[0].Kind != tc.kind || kinds[0].AdaptationSet != "0" || kinds[0].Period != "p0"):
				t.Errorf("warnings = %+v, want one %s for p0/0", kinds, tc.kind)
			case tc.kids != nil && !slices.Equal(kinds[0].KIDs, tc.kids):
				t.Errorf("warning KIDs = %v, want %v", kinds[0].KIDs, tc.kids)
			}
			if tc.sibling == nil && fetches > 0 {
				t.Errorf("fetched %d sibling inits for a single-representation AdaptationSet", fetches)
			}

			// Whatever was decided holds for the next representation.
			if tc.sibling != nil && !tc.fetchErr {
				next := task("r1", "r0")
				got, err := r.PrepareInit(context.Background(), next, tc.sibling, false, func(context.Context, queue.InitRef) ([]byte, error) {
					t.Error("a decided AdaptationSet fetched again")
					return nil, errors.New("unexpected")
				})
				if err != nil {
					t.Fatalf("second representation: %v", err)
				}
				if tc.decrypted && !isClear(t, got) {
					t.Error("second representation of a decrypted AdaptationSet got an encrypted init")
				}
				if !tc.decrypted && !bytes.Equal(got, tc.sibling) {
					t.Error("second representation of a kept AdaptationSet got a changed init")
				}
			}
		})
	}
}

// An AdaptationSet that already stores files, or that the index already
// holds media of, is kept as stored even when every key is held, so it never
// mixes ciphertext and clear media.
func TestPrepareInitKeepsAdaptationSetWithStoredMedia(t *testing.T) {
	video := readFixture(t, "cenc", "video", "avc1", "init.mp4")
	for _, tc := range []struct {
		name  string
		held  bool
		files bool
	}{
		{name: "index holds media", held: true},
		{name: "a representation directory holds a segment", files: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.files {
				loc := store.RepLocation{DataDir: dir, ChannelID: "c", PeriodID: "p0", MediaType: "video", ASID: "0", RepID: "other"}
				if err := store.Write(loc.SegmentPath(1, store.ExtFMP4), []byte("ciphertext")); err != nil {
					t.Fatal(err)
				}
			}
			r := NewRegistry(dir, "c", keySet(t, videoKey))
			got, err := r.PrepareInit(context.Background(), task("r0"), video, tc.held, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, video) || r.IsDecrypted(ASKey{"p0", "0"}) {
				t.Error("an AdaptationSet with stored media was decrypted")
			}
		})
	}
}

// A representation that joins a decrypted AdaptationSet later is decrypted
// when its key is held, and refused otherwise. A refusal changes the
// generation, is recorded on disk so a restart still knows it, and ends,
// with its missing_key warning, once an init of the representation is
// accepted.
func TestPrepareInitNewRepresentationOfDecryptedAdaptationSet(t *testing.T) {
	video := readFixture(t, "cenc", "video", "avc1", "init.mp4")
	audio := readFixture(t, "cenc", "audio", "und", "mp4a.40.2", "init.mp4")
	dir := t.TempDir()
	as := ASKey{"p0", "0"}
	r := NewRegistry(dir, "c", keySet(t, videoKey))
	clearVideo, err := r.PrepareInit(context.Background(), task("r0"), video, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Stored as ingest would, so Load finds the AdaptationSet.
	loc := store.RepLocation{DataDir: dir, ChannelID: "c", PeriodID: "p0", MediaType: "video", ASID: "0", RepID: "r0"}
	if err := store.Write(loc.InitPath(), clearVideo); err != nil {
		t.Fatal(err)
	}
	gen := r.Generation()
	if _, err := r.PrepareInit(context.Background(), task("r1"), audio, false, nil); err == nil {
		t.Fatal("a representation without a key joined a decrypted AdaptationSet")
	}
	if _, _, ok := r.Lookup(as, "r1"); ok {
		t.Error("the refused representation is looked up as decrypted")
	}
	if !r.IsRefused(as, "r1") || r.IsRefused(as, "r0") {
		t.Error("IsRefused does not report exactly the refused representation")
	}
	if r.Generation() == gen {
		t.Error("a refusal did not change the generation")
	}
	if got := r.Refused(); !slices.Equal(got, []RefusedRep{{AS: as, RepID: "r1"}}) {
		t.Errorf("Refused = %+v, want r1", got)
	}
	if ws := r.Warnings(); len(ws) != 1 || ws[0].Kind != KindMissingKey || !slices.Equal(ws[0].KIDs, []string{audioKID}) {
		t.Errorf("warnings = %+v, want missing_key for the audio KID", ws)
	}
	restarted := NewRegistry(dir, "c", keySet(t, videoKey))
	if err := restarted.Load(); err != nil {
		t.Fatal(err)
	}
	if !restarted.IsRefused(as, "r1") {
		t.Error("a restart forgot the refusal")
	}

	epoch := r.KeyEpoch()
	r.AddKeys([]cenc.Key{mustKey(t, audioKey)})
	if r.KeyEpoch() == epoch {
		t.Error("AddKeys did not change the key epoch")
	}
	gen = r.Generation()
	got, err := r.PrepareInit(context.Background(), task("r1"), audio, false, nil)
	if err != nil {
		t.Fatalf("with its key: %v", err)
	}
	if !isClear(t, got) {
		t.Error("the joining representation's init is still encrypted")
	}
	if _, _, ok := r.Lookup(as, "r1"); !ok {
		t.Error("the joining representation is not decrypted")
	}
	if r.IsRefused(as, "r1") || r.Generation() == gen {
		t.Error("accepting the refused representation did not end its refusal")
	}
	if ws := r.Warnings(); len(ws) != 0 {
		t.Errorf("warnings once accepted = %+v, want none", ws)
	}
	restarted = NewRegistry(dir, "c", keySet(t, videoKey, audioKey))
	if err := restarted.Load(); err != nil {
		t.Fatal(err)
	}
	if restarted.IsRefused(as, "r1") {
		t.Error("a restart brought back an ended refusal")
	}
}

func mustKey(t *testing.T, s string) cenc.Key {
	t.Helper()
	k, err := cenc.ParseKey(s)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// Load reads back decrypt.json, clears an init a crash left encrypted, and
// reports an encrypted AdaptationSet stored without one.
func TestLoadRestoresDecisions(t *testing.T) {
	dir := t.TempDir()
	video := readFixture(t, "cenc", "video", "avc1", "init.mp4")
	audio := readFixture(t, "cenc", "audio", "und", "mp4a.40.2", "init.mp4")
	keys := keySet(t, videoKey)

	first := NewRegistry(dir, "c", keys)
	if _, err := first.PrepareInit(context.Background(), task("v0"), video, false, nil); err != nil {
		t.Fatal(err)
	}
	// The crash: decrypt.json is written, the clear init is not.
	videoLoc := store.RepLocation{DataDir: dir, ChannelID: "c", PeriodID: "p0", MediaType: "video", ASID: "0", RepID: "v0"}
	if err := store.Write(videoLoc.InitPath(), video); err != nil {
		t.Fatal(err)
	}
	// An audio AdaptationSet stored encrypted: no key for it.
	audioLoc := store.RepLocation{DataDir: dir, ChannelID: "c", PeriodID: "p0", MediaType: "audio", ASID: "1", RepID: "a0"}
	if err := store.Write(audioLoc.InitPath(), audio); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(dir, "c", keys)
	gen := r.Generation()
	if err := r.Load(); err != nil {
		t.Fatal(err)
	}
	if r.Generation() == gen {
		t.Error("Load did not change the generation")
	}
	if !r.IsDecrypted(ASKey{"p0", "0"}) {
		t.Fatal("decrypted AdaptationSet not restored")
	}
	tp, _, ok := r.Lookup(ASKey{"p0", "0"}, "v0")
	if !ok || tp.KID.String() != videoKID {
		t.Errorf("Lookup = %+v, %v; want the video track", tp, ok)
	}
	if onDisk, _ := os.ReadFile(videoLoc.InitPath()); !isClear(t, onDisk) {
		t.Error("Load left the decrypted AdaptationSet's init encrypted")
	}
	if r.IsDecrypted(ASKey{"p0", "1"}) {
		t.Error("the audio AdaptationSet is decrypted")
	}
	if onDisk, _ := os.ReadFile(audioLoc.InitPath()); !bytes.Equal(onDisk, audio) {
		t.Error("Load changed a kept AdaptationSet's init")
	}
	ws := r.Warnings()
	if len(ws) != 1 || ws[0].Kind != KindMissingKey || ws[0].AdaptationSet != "1" || !slices.Equal(ws[0].KIDs, []string{audioKID}) {
		t.Errorf("warnings = %+v, want missing_key for the audio AdaptationSet", ws)
	}
	// A later representation of the kept AdaptationSet stays encrypted even
	// once the key is known.
	r.AddKeys([]cenc.Key{mustKey(t, audioKey)})
	next := &queue.SegmentTask{ChannelID: "c", PeriodID: "p0", ASID: "1", RepID: "a1", MediaType: "audio"}
	if got, err := r.PrepareInit(context.Background(), next, audio, false, nil); err != nil || !bytes.Equal(got, audio) {
		t.Errorf("new representation of a kept AdaptationSet: err %v, init changed %v", err, !bytes.Equal(got, audio))
	}
}

// rotKey's KID appears only in seig sample groups.
const (
	rotKey    = "40404040404040404040404040404040:404142434445464748494a4b4c4d4e4f"
	rotKIDHex = "40404040404040404040404040404040"
)

var rotKID = cenc.KID{0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40}

// rotationInit is init with an seig sample group in its stbl whose one entry
// protects samples under kid.
func rotationInit(t *testing.T, init []byte, kid cenc.KID) []byte {
	t.Helper()
	out, err := cenctest.Insert(init, []string{"moov", "trak", "mdia", "minf", "stbl"},
		cenctest.Sgpd([]cenctest.Group{{Protected: true, IVSize: 8, KID: kid}}))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A segment whose samples rotate to a KID without a key is withheld: the
// KID joins the AdaptationSet's missing_key warning and the representation
// is listed for a retry, until a segment decrypts with that key.
func TestSegmentRotationToMissingKey(t *testing.T) {
	r := NewRegistry(t.TempDir(), "c", keySet(t, videoKey, rotKey))
	if _, err := r.PrepareInit(context.Background(), task("v0"), readFixture(t, "cenc", "video", "avc1", "init.mp4"), false, nil); err != nil {
		t.Fatal(err)
	}
	as := ASKey{"p0", "0"}
	// The extra key is used only by segments, so far by none.
	if ws := r.Warnings(); len(ws) != 1 || ws[0].Kind != KindKeyUnmatched {
		t.Fatalf("warnings = %+v, want key_unmatched for the rotation key", ws)
	}

	missing := cenc.KID{0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50}
	r.RecordSegmentFailure(as, "v0", 7, &cenc.MissingKeyError{KIDs: []cenc.KID{missing}})
	ws := r.Warnings()
	if len(ws) != 2 || ws[0].Kind != KindMissingKey || !slices.Equal(ws[0].KIDs, []string{missing.String()}) {
		t.Fatalf("warnings = %+v, want missing_key for %s", ws, missing)
	}
	if got := r.Withheld(); !slices.Equal(got, []RefusedRep{{AS: as, RepID: "v0"}}) {
		t.Errorf("Withheld = %+v, want v0", got)
	}
	if !r.IsDecrypted(as) {
		t.Error("a withheld segment undid the decision")
	}

	// A segment using keys already held changes nothing but key_unmatched.
	r.SegmentDecrypted(as, []cenc.KID{rotKID})
	ws = r.Warnings()
	if len(ws) != 1 || ws[0].Kind != KindMissingKey || len(r.Withheld()) != 1 {
		t.Errorf("after a segment with held keys: warnings %+v, withheld %+v", ws, r.Withheld())
	}

	r.AddKeys([]cenc.Key{{KID: missing}})
	_, keys, _ := r.Lookup(as, "v0")
	if _, ok := keys(missing); !ok {
		t.Error("the KeyLookup Lookup returned misses a key added since")
	}
	r.SegmentDecrypted(as, []cenc.KID{missing})
	if ws := r.Warnings(); len(ws) != 0 {
		t.Errorf("warnings = %+v, want none once the KID decrypted", ws)
	}
	if got := r.Withheld(); len(got) != 0 {
		t.Errorf("Withheld = %+v, want none", got)
	}
}

// A decrypt.json written before key rotation was supported has no seig
// fields; it loads as before, needing tenc's key only.
func TestLoadStateWithoutSeigFields(t *testing.T) {
	dir := t.TempDir()
	loc := store.RepLocation{DataDir: dir, ChannelID: "c", PeriodID: "p0", MediaType: "video", ASID: "0", RepID: "v0"}
	if err := store.Write(loc.InitPath(), readFixture(t, "cenc", "video", "avc1", "init.mp4")); err != nil {
		t.Fatal(err)
	}
	old := `{"reps":{"v0":{"track_id":1,"scheme":"cenc","original_format":"avc1",` +
		`"kid":"10101010101010101010101010101010","is_protected":true,"per_sample_iv_size":16,` +
		`"crypt_blocks":0,"skip_blocks":0,"protected_entries":[1],` +
		`"default_sample_description_index":1,"default_sample_size":0}}}`
	if err := os.WriteFile(filepath.Join(loc.ASDir(), StateFileName), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(dir, "c", keySet(t, videoKey))
	if err := r.Load(); err != nil {
		t.Fatal(err)
	}
	tp, keys, ok := r.Lookup(ASKey{"p0", "0"}, "v0")
	if !ok || tp.SeigGroups != nil || !slices.Equal(tp.KIDs(), []cenc.KID{tp.KID}) {
		t.Fatalf("Lookup = %+v, %v", tp, ok)
	}
	seg := readFixture(t, "cenc", "video", "avc1", "seg-1.m4s")
	if err := cenc.DecryptSegment(seg, tp, keys); err != nil {
		t.Fatalf("DecryptSegment: %v", err)
	}
	if !bytes.Equal(mdat(t, seg), mdat(t, readFixture(t, "clear", "video", "avc1", "seg-1.m4s"))) {
		t.Error("segment decrypted with the old state differs from the clear one")
	}
	if ws := r.Warnings(); len(ws) != 0 {
		t.Errorf("warnings = %+v, want none", ws)
	}
}

// key_unmatched names a key no init uses, once an init has been read.
func TestWarningsKeyUnmatched(t *testing.T) {
	const unused = "30303030303030303030303030303030:0123456789abcdef0123456789abcdef"
	r := NewRegistry(t.TempDir(), "c", keySet(t, videoKey, unused))
	if ws := r.Warnings(); len(ws) != 0 {
		t.Errorf("before any init: %+v, want none", ws)
	}
	if _, err := r.PrepareInit(context.Background(), task("r0"), readFixture(t, "cenc", "video", "avc1", "init.mp4"), false, nil); err != nil {
		t.Fatal(err)
	}
	ws := r.Warnings()
	if len(ws) != 1 || ws[0].Kind != KindKeyUnmatched || !slices.Equal(ws[0].KIDs, []string{"30303030303030303030303030303030"}) {
		t.Errorf("warnings = %+v, want key_unmatched for the unused key only", ws)
	}
	for _, w := range ws {
		if strings.Contains(w.Message, "0123456789abcdef") {
			t.Errorf("warning holds a key value: %+v", w)
		}
	}
}
