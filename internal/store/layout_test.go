package store

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseSegmentFile(t *testing.T) {
	tests := []struct {
		name      string
		wantSegNo uint64
		wantExt   string
		wantOK    bool
	}{
		{"123.m4s", 123, ExtFMP4, true},
		{"000000001.m4s", 1, ExtFMP4, true},
		{"456.ts", 456, ExtTS, true},
		{"0.ts", 0, ExtTS, true},

		// Not segments.
		{"init.mp4", 0, "", false},
		{"123.mp4", 0, "", false},
		{"123", 0, "", false},
		{"abc.ts", 0, "", false},
		{".m4s", 0, "", false},
		{"", 0, "", false},
		{"12.3.ts", 0, "", false},
		{"-1.ts", 0, "", false},
	}

	for _, tc := range tests {
		segNo, ext, ok := ParseSegmentFile(tc.name)
		if ok != tc.wantOK {
			t.Errorf("ParseSegmentFile(%q) ok = %v, want %v", tc.name, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if segNo != tc.wantSegNo || ext != tc.wantExt {
			t.Errorf("ParseSegmentFile(%q) = (%d, %q), want (%d, %q)",
				tc.name, segNo, ext, tc.wantSegNo, tc.wantExt)
		}
	}
}

func TestSegmentPathRoundTrips(t *testing.T) {
	// The path builder and the filename parser must agree, since the rebuilder
	// and cleanup scanner discover on disk what the processor wrote.
	for _, ext := range []string{ExtFMP4, ExtTS} {
		p := RepLocation{DataDir: "/data", ChannelID: "ch1", PeriodID: "p0", MediaType: "video", ASID: "0", RepID: "720p"}.SegmentPath(42, ext)
		base := p[len("/data/ch1/periods/p0/video_0/720p/"):]
		segNo, gotExt, ok := ParseSegmentFile(base)
		if !ok {
			t.Fatalf("ParseSegmentFile(%q) failed for ext %q", base, ext)
		}
		if segNo != 42 || gotExt != ext {
			t.Errorf("round trip for %q = (%d, %q), want (42, %q)", ext, segNo, gotExt, ext)
		}
	}
}

// The layout on disk is what a rebuild after an upgrade reads back, so its
// shape must not drift. Positional AdaptationSet IDs encode as themselves.
func TestRepLocationLayout(t *testing.T) {
	l := RepLocation{DataDir: "/data", ChannelID: "ch1", PeriodID: "p0", MediaType: "video", ASID: "0", RepID: "720p"}
	for got, want := range map[string]string{
		l.SegmentPath(7, ExtFMP4): "/data/ch1/periods/p0/video_0/720p/7.m4s",
		l.InitPath():              "/data/ch1/periods/p0/video_0/720p/init.mp4",
		l.SidecarPath():           "/data/ch1/periods/p0/video_0/720p/segments.jsonl",
	} {
		if got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	}
}

// What RepLocation writes, ScanChannel must read back under the same IDs,
// including the IDs the encoding exists for (ADR-0006).
func TestScanChannelReadsBackWhatRepLocationWrote(t *testing.T) {
	data := t.TempDir()
	written := []RepLocation{
		{PeriodID: "", MediaType: "video", ASID: "0", RepID: "720p"},
		{PeriodID: "", MediaType: "audio", ASID: "1", RepID: "a/b"},
		{PeriodID: "p:1", MediaType: "video", ASID: "0", RepID: ".."},
		{PeriodID: "p:1", MediaType: "text", ASID: "x_y/z", RepID: "en"},
		// Encodes as "~Pz8_": the separator also appears inside the ID.
		{PeriodID: "p:1", MediaType: "text", ASID: "???", RepID: "fr"},
	}
	for i := range written {
		written[i].DataDir, written[i].ChannelID = data, "ch1"
		if err := os.MkdirAll(written[i].Dir(), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	periods, err := ScanChannel(data, "ch1")
	if err != nil {
		t.Fatal(err)
	}
	var got []RepLocation
	for _, p := range periods {
		for _, as := range p.AdaptationSets {
			for _, l := range as.Reps {
				if l.PeriodID != p.ID || l.ASID != as.ID || l.MediaType != as.MediaType {
					t.Errorf("%+v sits under period %q AS %q %q", l, p.ID, as.ID, as.MediaType)
				}
				got = append(got, l)
			}
		}
	}
	if !sameLocations(got, written) {
		t.Errorf("scanned %+v\nwant     %+v", got, written)
	}
}

func sameLocations(a, b []RepLocation) bool {
	set := func(ls []RepLocation) map[RepLocation]int {
		m := map[RepLocation]int{}
		for _, l := range ls {
			m[l]++
		}
		return m
	}
	return reflect.DeepEqual(set(a), set(b))
}

// A channel stored before the encoding existed has raw IDs as directory
// names; they still read back as those IDs (ADR-0006), even one that starts
// like an encoded name but does not decode.
func TestScanChannelReadsUnencodedNames(t *testing.T) {
	data := t.TempDir()
	for _, rep := range []string{"v:0", "~x"} {
		if err := os.MkdirAll(filepath.Join(PeriodsDir(data, "ch1"), "p:1", "video_0", rep), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	periods, err := ScanChannel(data, "ch1")
	if err != nil {
		t.Fatal(err)
	}
	want := []PeriodDir{{ID: "p:1", AdaptationSets: []ASDir{{ID: "0", MediaType: "video", Reps: []RepLocation{
		{DataDir: data, ChannelID: "ch1", PeriodID: "p:1", MediaType: "video", ASID: "0", RepID: "v:0"},
		{DataDir: data, ChannelID: "ch1", PeriodID: "p:1", MediaType: "video", ASID: "0", RepID: "~x"},
	}}}}}
	if !reflect.DeepEqual(periods, want) {
		t.Errorf("scanned %+v, want %+v", periods, want)
	}
}

// Empty directories are reported, files and names the layout never writes
// are skipped, and a channel with no media has no periods.
func TestScanChannelShape(t *testing.T) {
	data := t.TempDir()
	if got, err := ScanChannel(data, "none"); got != nil || err != nil {
		t.Fatalf("channel with no media: %+v, %v; want nil, nil", got, err)
	}
	base := PeriodsDir(data, "ch1")
	for _, dir := range []string{"0/video_0", "1", "0/novideo", "2/video_0", "3/audio_1/a0"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"stray", "0/stray", "0/video_0/stray"} {
		if err := os.WriteFile(filepath.Join(base, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A period or AdaptationSet that cannot be read is still reported.
	for _, dir := range []string{"2", "3/audio_1"} {
		unreadable := filepath.Join(base, dir)
		if err := os.Chmod(unreadable, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(unreadable, 0o755) })
		if _, err := os.ReadDir(unreadable); err == nil {
			t.Skip("running with permissions that read a mode-0 directory")
		}
	}
	got, err := ScanChannel(data, "ch1")
	if err != nil {
		t.Fatal(err)
	}
	want := []PeriodDir{
		{ID: "0", AdaptationSets: []ASDir{{ID: "0", MediaType: "video"}}},
		{ID: "1"},
		{ID: "2"},
		{ID: "3", AdaptationSets: []ASDir{{ID: "1", MediaType: "audio"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scanned %+v, want %+v", got, want)
	}
}
