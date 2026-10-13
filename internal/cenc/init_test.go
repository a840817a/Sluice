package cenc

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/validate"
)

func TestReadInitProtection(t *testing.T) {
	tests := []struct {
		variant string
		track   fixtureTrack
		ivSize  uint8
		constIV int
		crypt   uint8
		skip    uint8
	}{
		{variant: SchemeCENC, track: fixtureTracks[0], ivSize: 16},
		{variant: SchemeCENC, track: fixtureTracks[1], ivSize: 16},
		{variant: SchemeCBCS, track: fixtureTracks[0], constIV: 16, crypt: 1, skip: 9},
		{variant: SchemeCBCS, track: fixtureTracks[1], constIV: 16},
	}
	for _, tt := range tests {
		t.Run(tt.variant+"/"+tt.track.name, func(t *testing.T) {
			tp := fixtureProtection(t, tt.variant, tt.track)
			if tp.Scheme != tt.variant || tp.KID.String() != tt.track.kid || tp.TrackID != tt.track.trackID ||
				tp.OriginalFormat != tt.track.format || !tp.IsProtected {
				t.Errorf("got scheme %q kid %s track %d format %q protected %v, want %q %s %d %q true",
					tp.Scheme, tp.KID, tp.TrackID, tp.OriginalFormat, tp.IsProtected,
					tt.variant, tt.track.kid, tt.track.trackID, tt.track.format)
			}
			if tp.PerSampleIVSize != tt.ivSize || len(tp.ConstantIV) != tt.constIV {
				t.Errorf("IV size %d, constant IV %d bytes; want %d, %d", tp.PerSampleIVSize, len(tp.ConstantIV), tt.ivSize, tt.constIV)
			}
			if tp.CryptBlocks != tt.crypt || tp.SkipBlocks != tt.skip {
				t.Errorf("pattern %d:%d, want %d:%d", tp.CryptBlocks, tp.SkipBlocks, tt.crypt, tt.skip)
			}
			if !slices.Equal(tp.ProtectedEntries, []uint32{1}) || tp.DefaultSampleDescriptionIndex != 1 {
				t.Errorf("protected entries %v, trex sdi %d; want [1], 1", tp.ProtectedEntries, tp.DefaultSampleDescriptionIndex)
			}

			// A later phase persists this; it must survive JSON intact and
			// show the KID as hex.
			b, err := json.Marshal(tp)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(b), `"kid":"`+tt.track.kid+`"`) {
				t.Errorf("JSON %s does not carry the KID as hex", b)
			}
			var back TrackProtection
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(back, tp) {
				t.Errorf("JSON round trip = %+v, want %+v", back, tp)
			}
		})
	}

	t.Run("clear init", func(t *testing.T) {
		for _, tr := range fixtureTracks {
			tps, err := ReadInitProtection(readFixture(t, "clear", tr.dir, "init.mp4"))
			if tps != nil || err != nil {
				t.Errorf("%s: got %v, %v; want nil, nil", tr.name, tps, err)
			}
		}
	})
}

func TestReadInitProtectionErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, init []byte)
		want   error
	}{
		{
			name: "cens scheme",
			mutate: func(t *testing.T, init []byte) {
				schm := findBox(t, init, "moov", "trak", "mdia", "minf", "stbl", "stsd", "encv", "sinf", "schm")
				copy(init[schm.body+4:schm.body+8], "cens")
			},
			want: ErrUnsupportedScheme,
		},
		{
			name: "cbc1 scheme",
			mutate: func(t *testing.T, init []byte) {
				schm := findBox(t, init, "moov", "trak", "mdia", "minf", "stbl", "stsd", "encv", "sinf", "schm")
				copy(init[schm.body+4:schm.body+8], "cbc1")
			},
			want: ErrUnsupportedScheme,
		},
		{
			name: "seig sample group in stbl",
			mutate: func(t *testing.T, init []byte) {
				// stts is an empty full box: version, flags, entry_count.
				// Retyped, its entry_count becomes the grouping type.
				stts := findBox(t, init, "moov", "trak", "mdia", "minf", "stbl", "stts")
				putType(init, stts, "sgpd")
				copy(init[stts.body+4:stts.body+8], "seig")
			},
			want: ErrKeyRotation,
		},
		{
			name: "invalid tenc IV size",
			mutate: func(t *testing.T, init []byte) {
				tenc := findBox(t, init, "moov", "trak", "mdia", "minf", "stbl", "stsd", "encv", "sinf", "schi", "tenc")
				init[tenc.body+7] = 12 // default_Per_Sample_IV_Size
			},
			want: ErrMalformed,
		},
		{
			name: "box overruns moov",
			mutate: func(t *testing.T, init []byte) {
				trak := findBox(t, init, "moov", "trak")
				putU32(init, trak.start, uint32(len(init)))
			},
			want: ErrMalformed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			init := readFixture(t, SchemeCENC, fixtureTracks[0].dir, "init.mp4")
			tt.mutate(t, init)
			if _, err := ReadInitProtection(init); !errors.Is(err, tt.want) {
				t.Errorf("ReadInitProtection err = %v, want %v", err, tt.want)
			}
			if _, err := ClearInit(init); !errors.Is(err, tt.want) {
				t.Errorf("ClearInit err = %v, want %v", err, tt.want)
			}
		})
	}
}

// After ClearInit an init must look clear to a player: no encrypted sample
// entry, no sinf, no pssh, and the stsd the clear packaging would have had.
func TestClearInit(t *testing.T) {
	for _, variant := range encryptedVariants {
		for _, tr := range fixtureTracks {
			t.Run(variant+"/"+tr.name, func(t *testing.T) {
				enc := readFixture(t, variant, tr.dir, "init.mp4")
				clear := readFixture(t, "clear", tr.dir, "init.mp4")
				if !slices.Contains(boxTypes(t, enc), "pssh") {
					t.Fatal("fixture init has no pssh to drop")
				}

				out, err := ClearInit(enc)
				if err != nil {
					t.Fatalf("ClearInit: %v", err)
				}
				if err := validate.ValidateInit(out); err != nil {
					t.Errorf("ValidateInit: %v", err)
				}
				path := filepath.Join(t.TempDir(), "init.mp4")
				if err := os.WriteFile(path, out, 0o644); err != nil {
					t.Fatal(err)
				}
				got, err := validate.ReadInitTimescale(path)
				if err != nil {
					t.Fatalf("ReadInitTimescale: %v", err)
				}
				want, _ := validate.TimescaleFromInitBytes(clear)
				if got != want {
					t.Errorf("timescale = %d, want %d", got, want)
				}

				types := boxTypes(t, out)
				for _, banned := range []string{"encv", "enca", "sinf", "frma", "schm", "tenc", "pssh"} {
					if slices.Contains(types, banned) {
						t.Errorf("cleared init still has %s", banned)
					}
				}
				if !slices.Contains(types, tr.format) {
					t.Errorf("cleared init has no %s entry: %v", tr.format, types)
				}

				// The clear and encrypted fixtures come from the same
				// file, so the cleared stsd must equal the clear one.
				path = "moov/trak/mdia/minf/stbl/stsd"
				gotStsd := findBox(t, out, strings.Split(path, "/")...)
				wantStsd := findBox(t, clear, strings.Split(path, "/")...)
				if !bytes.Equal(out[gotStsd.start:gotStsd.end], clear[wantStsd.start:wantStsd.end]) {
					t.Errorf("cleared stsd differs from the clear fixture's")
				}

				again, err := ClearInit(out)
				if err != nil || !bytes.Equal(again, out) {
					t.Errorf("ClearInit is not idempotent (err %v)", err)
				}
				if tps, err := ReadInitProtection(out); tps != nil || err != nil {
					t.Errorf("cleared init reads as protected: %v, %v", tps, err)
				}
			})
		}
	}

	t.Run("clear init unchanged", func(t *testing.T) {
		clear := readFixture(t, "clear", fixtureTracks[0].dir, "init.mp4")
		out, err := ClearInit(clear)
		if err != nil || !bytes.Equal(out, clear) {
			t.Errorf("ClearInit(clear) changed it (err %v)", err)
		}
	})
}
