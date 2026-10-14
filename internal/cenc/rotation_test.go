package cenc

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/a840817a/sluice/internal/cenc/cenctest"
	"github.com/a840817a/sluice/internal/validate"
)

// Key rotation fixtures are built from the clear video by cenctest, which
// encrypts with crypto/aes directly: Bento4 1.6 cannot write seig groups.
var (
	rotKID1 = KID{0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10}
	rotKID2 = KID{0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40}
	rotKID3 = KID{0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50, 0x50}
	rotKeys = KeySet{
		rotKID1: {0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff},
		rotKID2: {0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49, 0x4a, 0x4b, 0x4c, 0x4d, 0x4e, 0x4f, 0x40},
		rotKID3: {0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x5b, 0x5c, 0x5d, 0x5e, 0x5f, 0x50},
	}
)

func rotationKeys() map[[16]byte][16]byte {
	out := make(map[[16]byte][16]byte)
	for k, v := range rotKeys {
		out[k] = v
	}
	return out
}

// rotationInit is the cenc video init with an seig sgpd in its stbl holding
// groups.
func rotationInit(t *testing.T, groups []cenctest.Group) []byte {
	t.Helper()
	init := readFixture(t, SchemeCENC, fixtureTracks[0].dir, "init.mp4")
	stbl := []string{"moov", "trak", "mdia", "minf", "stbl"}
	init, err := cenctest.Insert(init, stbl, cenctest.Sgpd(groups))
	if err != nil {
		t.Fatal(err)
	}
	// An empty sbgp too: the init has no samples to map.
	if init, err = cenctest.Insert(init, stbl, cenctest.Sbgp(nil)); err != nil {
		t.Fatal(err)
	}
	return init
}

// rotated encrypts clear video segment 1 as rot says, with tenc's defaults
// as the cenc fixture has them.
func rotated(t *testing.T, rot cenctest.Rotation) []byte {
	t.Helper()
	rot.Default = cenctest.Group{Protected: true, IVSize: 16, KID: rotKID1}
	rot.Keys = rotationKeys()
	seg, err := cenctest.Rotate(readFixture(t, "clear", fixtureTracks[0].dir, segName(1)), rot)
	if err != nil {
		t.Fatalf("build rotated segment: %v", err)
	}
	if bytes.Equal(mdatPayload(t, seg), mdatPayload(t, readFixture(t, "clear", fixtureTracks[0].dir, segName(1)))) {
		t.Fatal("rotated segment is not encrypted")
	}
	return seg
}

// indexes maps the fixture segment's 12 samples to groups, a run of each.
func indexes(runs ...uint32) []uint32 {
	var out []uint32
	for i := range 12 {
		out = append(out, runs[i*len(runs)/12])
	}
	return out
}

func assertClearSamples(t *testing.T, seg []byte, encLen int) {
	t.Helper()
	want := mdatPayload(t, readFixture(t, "clear", fixtureTracks[0].dir, segName(1)))
	if got := mdatPayload(t, seg); !bytes.Equal(got, want) {
		t.Errorf("decrypted mdat differs from clear at byte %d of %d", firstDiff(got, want), len(want))
	}
	if len(seg) != encLen {
		t.Errorf("length %d, want %d", len(seg), encLen)
	}
	if _, err := validate.ValidateSegment(seg); err != nil {
		t.Errorf("ValidateSegment: %v", err)
	}
	for _, typ := range boxTypes(t, seg) {
		if typ == "senc" || typ == "sgpd" || typ == "sbgp" {
			t.Errorf("decrypted segment still has %s", typ)
		}
	}
}

// A fragment that moves samples to KIDs of its own sgpd decrypts each with
// its group's key and IV size; samples in no group keep tenc's.
func TestDecryptSegmentRotationFragmentGroups(t *testing.T) {
	tp := fixtureProtection(t, SchemeCENC, fixtureTracks[0])
	seg := rotated(t, cenctest.Rotation{
		Local: []cenctest.Group{
			{Protected: true, IVSize: 8, KID: rotKID2},
			{Protected: true, IVSize: 16, KID: rotKID3},
		},
		Index: indexes(0, cenctest.LocalBase+1, cenctest.LocalBase+2, cenctest.LocalBase+1),
	})
	before := readLayout(t, seg)
	if err := DecryptSegment(seg, tp, rotKeys.Lookup); err != nil {
		t.Fatalf("DecryptSegment: %v", err)
	}
	assertClearSamples(t, seg, before.length)
	if after := readLayout(t, seg); !layoutEqual(before, after) {
		t.Errorf("layout changed: before %+v, after %+v", before, after)
	}
}

// The init's seig entries are numbered from 1 and the fragment's from
// 0x10001; a fragment can use both.
func TestDecryptSegmentRotationInitGroups(t *testing.T) {
	tps, err := ReadInitProtection(rotationInit(t, []cenctest.Group{
		{Protected: true, IVSize: 8, KID: rotKID3},
		{Protected: true, IVSize: 16, KID: rotKID2},
	}))
	if err != nil || len(tps) != 1 {
		t.Fatalf("ReadInitProtection: %v, %v", tps, err)
	}
	tp := tps[0]
	seg := rotated(t, cenctest.Rotation{
		Init: []cenctest.Group{
			{Protected: true, IVSize: 8, KID: rotKID3},
			{Protected: true, IVSize: 16, KID: rotKID2},
		},
		Local: []cenctest.Group{{Protected: true, IVSize: 8, KID: rotKID2}},
		Index: indexes(1, cenctest.LocalBase+1, 2, 0, 1, cenctest.LocalBase+1),
	})
	n := len(seg)
	if err := DecryptSegment(seg, tp, rotKeys.Lookup); err != nil {
		t.Fatalf("DecryptSegment: %v", err)
	}
	assertClearSamples(t, seg, n)
}

// A seig entry with isProtected 0 leaves its samples as they are.
func TestDecryptSegmentRotationClearGroup(t *testing.T) {
	tp := fixtureProtection(t, SchemeCENC, fixtureTracks[0])
	seg := rotated(t, cenctest.Rotation{
		Local: []cenctest.Group{{Protected: false, KID: rotKID3}, {Protected: true, IVSize: 8, KID: rotKID2}},
		Index: indexes(cenctest.LocalBase+1, cenctest.LocalBase+2, cenctest.LocalBase+1),
	})
	// rotKID3's key is not needed: its group is clear.
	keys := KeySet{rotKID2: rotKeys[rotKID2]}
	n := len(seg)
	if err := DecryptSegment(seg, tp, keys.Lookup); err != nil {
		t.Fatalf("DecryptSegment: %v", err)
	}
	assertClearSamples(t, seg, n)
}

// A sample whose KID has no key fails the segment with the KIDs missing,
// and nothing of it is decrypted.
func TestDecryptSegmentRotationMissingKey(t *testing.T) {
	tp := fixtureProtection(t, SchemeCENC, fixtureTracks[0])
	seg := rotated(t, cenctest.Rotation{
		Local: []cenctest.Group{{Protected: true, IVSize: 8, KID: rotKID2}, {Protected: true, IVSize: 8, KID: rotKID3}},
		Index: indexes(0, cenctest.LocalBase+2, cenctest.LocalBase+1),
	})
	orig := bytes.Clone(seg)
	err := DecryptSegment(seg, tp, KeySet{rotKID1: rotKeys[rotKID1]}.Lookup)
	var mk *MissingKeyError
	if !errors.As(err, &mk) || !errors.Is(err, ErrMissingKey) {
		t.Fatalf("err = %v, want a MissingKeyError", err)
	}
	if !slices.Equal(mk.KIDs, []KID{rotKID2, rotKID3}) {
		t.Errorf("missing KIDs = %v, want %v and %v", mk.KIDs, rotKID2, rotKID3)
	}
	if !bytes.Equal(seg, orig) {
		t.Error("segment modified on error")
	}
	for _, k := range rotKeys {
		assertNoKey(t, err.Error(), k)
	}
}

// An sbgp index past the entries there are is not guessed at.
func TestDecryptSegmentRotationBadIndex(t *testing.T) {
	tp := fixtureProtection(t, SchemeCENC, fixtureTracks[0])
	seg := rotated(t, cenctest.Rotation{
		Local: []cenctest.Group{{Protected: true, IVSize: 8, KID: rotKID2}},
		Index: indexes(cenctest.LocalBase + 1),
	})
	// Retarget the run to entry 2 of an init that has none.
	sbgp := findBox(t, seg, "moof", "traf", "sbgp")
	putU32(seg, sbgp.end-4, 2)
	orig := bytes.Clone(seg)
	if err := DecryptSegment(seg, tp, rotKeys.Lookup); !errors.Is(err, ErrUnsupportedScheme) {
		t.Fatalf("err = %v, want ErrUnsupportedScheme", err)
	}
	if !bytes.Equal(seg, orig) {
		t.Error("segment modified on error")
	}
}

// ReadInitProtection reports the init's seig entries, so the KIDs an
// AdaptationSet needs keys for include theirs; ClearInit drops the groups.
func TestInitRotationGroups(t *testing.T) {
	init := rotationInit(t, []cenctest.Group{
		{Protected: true, IVSize: 8, KID: rotKID2},
		{Protected: false, KID: rotKID3},
		{Protected: true, IVSize: 16, KID: rotKID1},
	})
	tps, err := ReadInitProtection(init)
	if err != nil || len(tps) != 1 {
		t.Fatalf("ReadInitProtection: %v, %v", tps, err)
	}
	tp := tps[0]
	want := []SeigEntry{
		{IsProtected: true, PerSampleIVSize: 8, KID: rotKID2},
		{KID: rotKID3},
		{IsProtected: true, PerSampleIVSize: 16, KID: rotKID1},
	}
	if !slices.EqualFunc(tp.SeigGroups, want, func(a, b SeigEntry) bool {
		return a.IsProtected == b.IsProtected && a.PerSampleIVSize == b.PerSampleIVSize && a.KID == b.KID
	}) {
		t.Errorf("SeigGroups = %+v, want %+v", tp.SeigGroups, want)
	}
	if got := tp.KIDs(); !slices.Equal(got, []KID{rotKID1, rotKID2}) {
		t.Errorf("KIDs = %v, want tenc's and the protected entry's", got)
	}

	out, err := ClearInit(init)
	if err != nil {
		t.Fatalf("ClearInit: %v", err)
	}
	types := boxTypes(t, out)
	for _, banned := range []string{"sgpd", "sbgp", "encv", "sinf"} {
		if slices.Contains(types, banned) {
			t.Errorf("cleared init still has %s", banned)
		}
	}
	if err := validate.ValidateInit(out); err != nil {
		t.Errorf("ValidateInit: %v", err)
	}

	// A TrackProtection persisted before key rotation was supported has no
	// seig fields and reads back with none.
	var old TrackProtection
	if err := json.Unmarshal([]byte(`{"track_id":1,"scheme":"cenc","kid":"10101010101010101010101010101010","is_protected":true,"per_sample_iv_size":16}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.SeigGroups != nil || old.SeigDefault != 0 || !slices.Equal(old.KIDs(), []KID{rotKID1}) {
		t.Errorf("old TrackProtection = %+v", old)
	}
}

// A malformed seig entry in the init makes the track unsupported.
func TestInitRotationMalformedEntry(t *testing.T) {
	init := rotationInit(t, []cenctest.Group{{Protected: true, IVSize: 12, KID: rotKID2}})
	if _, err := ReadInitProtection(init); !errors.Is(err, ErrUnsupportedScheme) {
		t.Errorf("err = %v, want ErrUnsupportedScheme", err)
	}
}
