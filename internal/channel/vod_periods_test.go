package channel

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/store"
)

func periodsMPD(t *testing.T, ids ...string) (*imdp.ParsedMPD, []byte) {
	t.Helper()
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT10S">`)
	for _, id := range ids {
		fmt.Fprintf(&b, `<Period id="%s"><AdaptationSet mimeType="video/mp4"><SegmentTemplate timescale="1" duration="2" media="$Number$.m4s"/><Representation id="v-%s" bandwidth="1"/></AdaptationSet></Period>`, id, id)
	}
	b.WriteString(`</MPD>`)
	raw := []byte(b.String())
	p, err := imdp.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p, raw
}

// vodOriginMPD describes each stored period from the copy saved in its
// directory, falls back to the last MPD's period of the same origin Period
// ID, and leaves out, with a warning, a period it can describe neither way.
func TestVODOriginMPDDescribesEveryStoredPeriod(t *testing.T) {
	for _, tc := range []struct {
		name      string
		final     []string // periods the origin's last MPD lists
		stored    []string // periods the VOD index holds
		saved     []string // periods with a saved origin MPD listing them
		want      string   // period ID: representation ID, in order
		wantWarns int
	}{
		{name: "all listed", final: []string{"a", "b"}, stored: []string{"a", "b"}, want: "a:v-a b:v-b"},
		{name: "rolled off, saved", final: []string{"b"}, stored: []string{"a", "b"}, saved: []string{"a"}, want: "a:v-a b:v-b"},
		{name: "restart, saved", final: []string{"a.r1"}, stored: []string{"a", "a.r1"}, saved: []string{"a"}, want: "a:v-a a.r1:v-a"},
		{name: "restart, not saved", final: []string{"a.r1"}, stored: []string{"a", "a.r1"}, want: "a:v-a a.r1:v-a"},
		{name: "rolled off, not saved", final: []string{"b"}, stored: []string{"a", "b"}, want: "b:v-b", wantWarns: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			// The final MPD names its periods as the watcher renamed them; a
			// saved copy holds the origin's own IDs.
			final, _ := periodsMPD(t, tc.final...)
			for _, p := range final.Periods {
				if p.ID == "a.r1" {
					p.AdaptationSets[0].Representations[0].ID = "v-a"
				}
			}
			for _, id := range tc.saved {
				_, raw := periodsMPD(t, index.OriginPeriodID(id))
				path := store.PeriodOriginMPDPath(dataDir, "ch", id)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			ci := index.NewChannelIndex(index.PublishAVBarrier)
			for _, id := range tc.stored {
				ci.Period(id).AS("0").Rep("v", 0).RestorePublished(index.SegmentState{SegNo: 1, StartPTS: 0, EndPTS: 2, Timescale: 1})
			}
			before := len(final.Periods)

			got, warnings := vodOriginMPD(final, ci.Snapshot(), dataDir, "ch")

			var desc []string
			for _, p := range got.Periods {
				desc = append(desc, p.ID+":"+p.AdaptationSets[0].Representations[0].ID)
			}
			if strings.Join(desc, " ") != tc.want {
				t.Errorf("periods = %v, want %s", desc, tc.want)
			}
			if len(warnings) != tc.wantWarns {
				t.Errorf("warnings = %q, want %d", warnings, tc.wantWarns)
			}
			if len(final.Periods) != before {
				t.Errorf("final MPD was modified: %d periods, had %d", len(final.Periods), before)
			}
		})
	}
}
