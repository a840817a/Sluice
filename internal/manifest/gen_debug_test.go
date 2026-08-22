package manifest

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	gompeg "github.com/unki2aut/go-mpd"
	xsd "github.com/unki2aut/go-xsd-types"
	"time"
)

func TestStubFormat(t *testing.T) {
	data, _ := os.ReadFile("../../testdata/manifest.mpd")
	p, _ := imdp.Parse(data)

	// Replicate just the MPD build without post-processing
	m := &gompeg.MPD{Profiles: "urn:mpeg:dash:profile:isoff-main:2011"}
	mpdType := "static"
	m.Type = &mpdType
	xmlns := "urn:mpeg:dash:schema:mpd:2011"
	m.XMLNS = &xmlns
	dur := xsd.Duration{Seconds: 30}
	m.MediaPresentationDuration = &dur
	minBuf := xsd.Duration{Seconds: 2}
	m.MinBufferTime = &minBuf
	_ = time.Now()

	for _, origPeriod := range p.Periods {
		_ = index.NewChannelIndex()
		period := &gompeg.Period{}
		if origPeriod.ID != "" {
			period.ID = &origPeriod.ID
		}
		for _, origAS := range origPeriod.AdaptationSets {
			as := &gompeg.AdaptationSet{MimeType: origAS.MimeType}
			as.ContentProtections = origAS.ContentProtections
			period.AdaptationSets = append(period.AdaptationSets, as)
		}
		m.Period = append(m.Period, period)
	}

	encoded, _ := m.Encode()
	encoded = injectNamespaces(encoded)

	xml := string(encoded)
	// Print just the ContentProtection lines
	for _, line := range strings.Split(xml, "\n") {
		if strings.Contains(line, "ContentProtection") {
			fmt.Printf("STUB: %q\n", strings.TrimSpace(line))
		}
	}
}
