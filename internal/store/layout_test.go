package store

import "testing"

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
// shape must not drift.
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
