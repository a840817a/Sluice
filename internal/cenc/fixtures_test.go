package cenc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mp4 "github.com/abema/go-mp4"
)

// The fixtures in testdata/cenc are one clip packaged three times from the same
// fragmented file (see its README), so an encrypted segment and the clear
// segment of the same number hold the same samples.
const fixtureDir = "../../testdata/cenc"

type fixtureTrack struct {
	name     string
	dir      string // representation directory inside a variant
	trackID  uint32
	segments int
	format   string // the frma original format
	kid      string
}

var fixtureTracks = []fixtureTrack{
	{name: "video", dir: "video/avc1", trackID: 1, segments: 4, format: "avc1", kid: "10101010101010101010101010101010"},
	{name: "audio", dir: "audio/und/mp4a.40.2", trackID: 2, segments: 5, format: "mp4a", kid: "20202020202020202020202020202020"},
}

var encryptedVariants = []string{SchemeCENC, SchemeCBCS}

func readFixture(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{fixtureDir}, parts...)...))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func segName(n int) string { return fmt.Sprintf("seg-%d.m4s", n) }

// fixtureKeys reads keys.txt the way an operator would type the keys.
func fixtureKeys(t *testing.T) KeySet {
	t.Helper()
	ks := KeySet{}
	for _, line := range strings.Split(string(readFixture(t, "keys.txt")), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, err := ParseKey(line)
		if err != nil {
			t.Fatalf("keys.txt: %v", err)
		}
		ks[k.KID] = k.Value
	}
	if len(ks) != 2 {
		t.Fatalf("keys.txt has %d keys, want 2", len(ks))
	}
	return ks
}

// fixtureProtection reads the one protected track of a fixture init.
func fixtureProtection(t *testing.T, variant string, tr fixtureTrack) TrackProtection {
	t.Helper()
	tps, err := ReadInitProtection(readFixture(t, variant, tr.dir, "init.mp4"))
	if err != nil {
		t.Fatalf("ReadInitProtection: %v", err)
	}
	if len(tps) != 1 {
		t.Fatalf("%d protected tracks, want 1", len(tps))
	}
	return tps[0]
}

// mdatPayload concatenates the payloads of a segment's top-level mdat boxes.
func mdatPayload(t *testing.T, seg []byte) []byte {
	t.Helper()
	top, err := boxes(seg, 0, len(seg))
	if err != nil {
		t.Fatalf("boxes: %v", err)
	}
	var out []byte
	for _, b := range top {
		if b.typ == "mdat" {
			out = append(out, seg[b.body:b.end]...)
		}
	}
	if len(out) == 0 {
		t.Fatal("segment has no mdat payload")
	}
	return out
}

// layout is what in-place decryption promises not to change, read with
// go-mp4 rather than this package's own parser.
type layout struct {
	length      int
	moofSizes   []uint64
	dataOffsets []int32
}

func readLayout(t *testing.T, seg []byte) layout {
	t.Helper()
	l := layout{length: len(seg)}
	moofs, err := mp4.ExtractBox(bytes.NewReader(seg), nil, mp4.BoxPath{mp4.BoxTypeMoof()})
	if err != nil {
		t.Fatalf("extract moof: %v", err)
	}
	for _, m := range moofs {
		l.moofSizes = append(l.moofSizes, m.Size)
	}
	truns, err := mp4.ExtractBoxWithPayload(bytes.NewReader(seg), nil,
		mp4.BoxPath{mp4.BoxTypeMoof(), mp4.BoxTypeTraf(), mp4.BoxTypeTrun()})
	if err != nil {
		t.Fatalf("extract trun: %v", err)
	}
	for _, tr := range truns {
		l.dataOffsets = append(l.dataOffsets, tr.Payload.(*mp4.Trun).DataOffset)
	}
	if len(l.moofSizes) == 0 || len(l.dataOffsets) == 0 {
		t.Fatal("segment has no moof or trun")
	}
	return l
}

// boxTypes lists the type of every box in data, depth first, descending into
// the containers this package touches.
func boxTypes(t *testing.T, data []byte) []string {
	t.Helper()
	var out []string
	var walk func(start, end int)
	walk = func(start, end int) {
		list, err := boxes(data, start, end)
		if err != nil {
			t.Fatalf("boxes: %v", err)
		}
		for _, b := range list {
			out = append(out, b.typ)
			switch b.typ {
			case "moov", "trak", "mdia", "minf", "stbl", "moof", "traf", "sinf", "schi", "mvex":
				walk(b.body, b.end)
			case "stsd":
				walk(b.body+8, b.end)
			case "encv", "avc1":
				walk(b.body+78, b.end)
			case "enca", "mp4a":
				walk(b.body+28, b.end)
			}
		}
	}
	walk(0, len(data))
	return out
}

// findBox returns the first box of type typ anywhere boxTypes would reach.
func findBox(t *testing.T, data []byte, path ...string) box {
	t.Helper()
	start, end := 0, len(data)
	var b box
	for i, typ := range path {
		list, err := boxes(data, start, end)
		if err != nil {
			t.Fatalf("boxes: %v", err)
		}
		var ok bool
		if b, ok = child(list, typ); !ok {
			t.Fatalf("no %s in %v", typ, path[:i])
		}
		start, end = b.body, b.end
		switch typ {
		case "stsd":
			start += 8
		case "encv":
			start += 78
		case "enca":
			start += 28
		}
	}
	return b
}

func putType(data []byte, b box, typ string) { copy(data[b.start+4:b.start+8], typ) }

func putU32(data []byte, off int, v uint32) { binary.BigEndian.PutUint32(data[off:], v) }

// onlyKey is a KeyLookup that answers key for every KID.
func onlyKey(key [16]byte) KeyLookup {
	return func(KID) ([16]byte, bool) { return key, true }
}
