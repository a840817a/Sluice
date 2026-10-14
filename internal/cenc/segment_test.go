package cenc

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/a840817a/sluice/internal/validate"
)

// Every encrypted fixture segment must decrypt to exactly the clear variant's
// samples, without moving a single box: the gateway serves the result with the
// origin's byte ranges and trun offsets.
func TestDecryptSegmentMatchesClear(t *testing.T) {
	keys := fixtureKeys(t)
	for _, variant := range encryptedVariants {
		for _, tr := range fixtureTracks {
			tp := fixtureProtection(t, variant, tr)
			key, ok := keys[tp.KID]
			if !ok {
				t.Fatalf("%s %s: no key for KID %s", variant, tr.name, tp.KID)
			}
			for n := 1; n <= tr.segments; n++ {
				t.Run(variant+"/"+tr.name+"/"+segName(n), func(t *testing.T) {
					seg := readFixture(t, variant, tr.dir, segName(n))
					clear := readFixture(t, "clear", tr.dir, segName(n))
					want := mdatPayload(t, clear)
					if bytes.Equal(mdatPayload(t, seg), want) {
						t.Fatal("fixture segment is not encrypted")
					}
					before := readLayout(t, seg)

					if err := DecryptSegment(seg, tp, onlyKey(key)); err != nil {
						t.Fatalf("DecryptSegment: %v", err)
					}

					if got := mdatPayload(t, seg); !bytes.Equal(got, want) {
						t.Errorf("decrypted mdat differs from clear at byte %d of %d", firstDiff(got, want), len(want))
					}
					if after := readLayout(t, seg); !layoutEqual(before, after) {
						t.Errorf("layout changed: before %+v, after %+v", before, after)
					}
					if _, err := validate.ValidateSegment(seg); err != nil {
						t.Errorf("ValidateSegment: %v", err)
					}
					for _, typ := range boxTypes(t, seg) {
						if typ == "senc" || typ == "saiz" || typ == "saio" || typ == "uuid" {
							t.Errorf("decrypted segment still has %s", typ)
						}
					}
				})
			}
		}
	}
}

func layoutEqual(a, b layout) bool {
	return a.length == b.length && slices.Equal(a.moofSizes, b.moofSizes) && slices.Equal(a.dataOffsets, b.dataOffsets)
}

func firstDiff(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// A wrong key cannot be told from a right one: CTR and CBC decrypt any bytes.
// The samples come out as garbage and no error is returned.
func TestDecryptSegmentWrongKeyIsUndetectable(t *testing.T) {
	for _, variant := range encryptedVariants {
		t.Run(variant, func(t *testing.T) {
			tr := fixtureTracks[0]
			tp := fixtureProtection(t, variant, tr)
			seg := readFixture(t, variant, tr.dir, segName(1))
			var wrong [16]byte
			if err := DecryptSegment(seg, tp, onlyKey(wrong)); err != nil {
				t.Fatalf("DecryptSegment with a wrong key: %v, want nil", err)
			}
			if bytes.Equal(mdatPayload(t, seg), mdatPayload(t, readFixture(t, "clear", tr.dir, segName(1)))) {
				t.Error("wrong key produced the clear samples")
			}
		})
	}
}

// A clear lead is a run of fragments that use a clear sample entry of an
// encrypted track. Bento4 cannot produce one, so the cases are built from the
// fixtures by changing which entries count as protected.
func TestDecryptSegmentClearLead(t *testing.T) {
	keys := fixtureKeys(t)
	tr := fixtureTracks[0]
	tp := fixtureProtection(t, SchemeCBCS, tr)
	key := keys[tp.KID]

	t.Run("fragment using a clear entry is untouched", func(t *testing.T) {
		lead := tp
		lead.ProtectedEntries = []uint32{2} // the fixture's fragments use entry 1
		seg := readFixture(t, SchemeCBCS, tr.dir, segName(1))
		orig := bytes.Clone(seg)
		if err := DecryptSegment(seg, lead, onlyKey(key)); err != nil {
			t.Fatalf("DecryptSegment: %v", err)
		}
		if !bytes.Equal(seg, orig) {
			t.Error("clear-lead fragment was modified")
		}
	})

	t.Run("clear samples under a protected entry need senc", func(t *testing.T) {
		seg := readFixture(t, "clear", tr.dir, segName(1))
		orig := bytes.Clone(seg)
		err := DecryptSegment(seg, tp, onlyKey(key))
		if !errors.Is(err, ErrNoSampleEncryption) {
			t.Fatalf("err = %v, want ErrNoSampleEncryption", err)
		}
		if !bytes.Equal(seg, orig) {
			t.Error("segment modified on error")
		}
	})
}

// Each case corrupts a fixture the way a broken or unsupported origin would.
// None may decrypt anything: a failed segment must come back as it was.
func TestDecryptSegmentErrors(t *testing.T) {
	keys := fixtureKeys(t)
	tr := fixtureTracks[0]

	tests := []struct {
		name    string
		variant string
		mutate  func(t *testing.T, seg []byte, tp *TrackProtection)
		want    error
	}{
		{
			name:    "truncated senc",
			variant: SchemeCENC,
			mutate: func(t *testing.T, seg []byte, _ *TrackProtection) {
				// Shrink senc by 16 bytes and fill the gap with a free box,
				// so the boxes around it still parse.
				senc := findBox(t, seg, "moof", "traf", "senc")
				size := senc.end - senc.start
				putU32(seg, senc.start, uint32(size-16))
				putU32(seg, senc.end-16, 16)
				copy(seg[senc.end-12:senc.end-8], "free")
			},
			want: ErrMalformed,
		},
		{
			name:    "IV size does not match senc",
			variant: SchemeCENC,
			mutate: func(_ *testing.T, _ []byte, tp *TrackProtection) {
				tp.PerSampleIVSize = 8 // the fixture's IVs are 16 bytes
			},
			want: ErrMalformed,
		},
		{
			name:    "senc sample count differs from trun",
			variant: SchemeCENC,
			mutate: func(t *testing.T, seg []byte, _ *TrackProtection) {
				senc := findBox(t, seg, "moof", "traf", "senc")
				putU32(seg, senc.body+4, 11)
			},
			want: ErrMalformed,
		},
		{
			name:    "malformed seig sample group in traf",
			variant: SchemeCBCS,
			mutate: func(t *testing.T, seg []byte, _ *TrackProtection) {
				saio := findBox(t, seg, "moof", "traf", "saio")
				putType(seg, saio, "sgpd")
				copy(seg[saio.body+4:saio.body+8], "seig")
			},
			want: ErrUnsupportedScheme,
		},
		{
			name:    "protected fragment without senc",
			variant: SchemeCBCS,
			mutate: func(t *testing.T, seg []byte, _ *TrackProtection) {
				putType(seg, findBox(t, seg, "moof", "traf", "senc"), "skip")
			},
			want: ErrNoSampleEncryption,
		},
		{
			name:    "no fragment for the track",
			variant: SchemeCBCS,
			mutate: func(_ *testing.T, _ []byte, tp *TrackProtection) {
				tp.TrackID = 9
			},
			want: ErrMalformed,
		},
		{
			name:    "trun points past mdat",
			variant: SchemeCENC,
			mutate: func(t *testing.T, seg []byte, _ *TrackProtection) {
				trun := findBox(t, seg, "moof", "traf", "trun")
				putU32(seg, trun.body+8, uint32(len(seg))) // data_offset
			},
			want: ErrMalformed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tp := fixtureProtection(t, tt.variant, tr)
			key := keys[tp.KID]
			seg := readFixture(t, tt.variant, tr.dir, segName(1))
			tt.mutate(t, seg, &tp)
			orig := bytes.Clone(seg)

			err := DecryptSegment(seg, tp, onlyKey(key))
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if !bytes.Equal(seg, orig) {
				t.Error("segment modified on error")
			}
			assertNoKey(t, err.Error(), key)
		})
	}
}
