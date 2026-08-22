package mpd

import (
	"fmt"
	"os"
	"testing"
)

func TestParseManifest(t *testing.T) {
	data, err := os.ReadFile("../../testdata/manifest.mpd")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	p, err := Parse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	fmt.Printf("Type: %s\n", p.Type)
	fmt.Printf("Duration: %s\n", p.MediaPresentationDuration)
	fmt.Printf("Periods: %d\n", len(p.Periods))

	for _, period := range p.Periods {
		fmt.Printf("  Period %q duration=%s\n", period.ID, period.Duration)
		for _, as := range period.AdaptationSets {
			fmt.Printf("    AdaptationSet %q type=%s mime=%s reps=%d\n", as.ID, as.MediaType, as.MimeType, len(as.Representations))
			for _, rep := range as.Representations {
				tmpl := rep.EffectiveTemplate(as)
				if tmpl == nil {
					t.Errorf("rep %q has no segment template", rep.ID)
					continue
				}
				lastSeg := tmpl.LastSegmentNumber(period.Duration)
				fmt.Printf("      Rep %q bw=%d segDur=%s last=%d\n", rep.ID, rep.Bandwidth, tmpl.SegmentDuration(), lastSeg)
				fmt.Printf("        initURL: %s\n", tmpl.InitURL(rep.ID))
				fmt.Printf("        seg1URL: %s\n", tmpl.SegmentURL(1, 0, rep.ID))
				fmt.Printf("        seg%dURL: %s\n", lastSeg, tmpl.SegmentURL(lastSeg, 0, rep.ID))

				if lastSeg == 0 {
					t.Errorf("rep %q: lastSeg=0", rep.ID)
				}
				if tmpl.InitURL(rep.ID) == "" {
					t.Errorf("rep %q: empty initURL", rep.ID)
				}
			}
		}
	}

	if p.Type != PresentationStatic {
		t.Errorf("expected static, got %s", p.Type)
	}
	if len(p.Periods) != 1 {
		t.Errorf("expected 1 period, got %d", len(p.Periods))
	}
}
