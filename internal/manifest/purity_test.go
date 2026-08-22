package manifest

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

func testMPD(t *testing.T) *imdp.ParsedMPD {
	t.Helper()
	data, err := os.ReadFile("../../testdata/manifest.mpd")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	p, err := imdp.Parse(data)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return p
}

// Serving a manifest must leave the index exactly as it found it.
//
// It used to not: the period loop called ci.Period(id), which takes the channel
// index's WRITE lock and inserts an empty PeriodState for every period the
// upstream declares. That made GET manifest.mpd a mutating request — one that
// changed what GET health subsequently reported — and it took a write lock on
// the hot serving path for no benefit, since the lookups that follow create
// nothing and tolerate a missing period.
//
// Generate can no longer do this even in principle: it takes an index.Snapshot
// and never sees the index. What remains worth asserting is the step that does
// still touch it — taking the snapshot must be read-only too.
func TestSnapshotAndGenerateLeaveIndexUntouched(t *testing.T) {
	p := testMPD(t)
	ci := index.NewChannelIndex()

	if before := ci.PeriodIDs(); len(before) != 0 {
		t.Fatalf("fresh index already has periods %v", before)
	}

	snap := ci.Snapshot()
	if _, err := Generate(p, snap, GeneratorConfig{
		GatewayBaseURL: "https://gw.example.com",
		ChannelID:      "ch1",
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}

	if after := ci.PeriodIDs(); len(after) != 0 {
		t.Errorf("serving a manifest created %d period(s) in the index: %v\n"+
			"A read path must not mutate the index — it changes what "+
			"/health reports and takes a write lock on the serving path.",
			len(after), after)
	}
}

// With the clock pinned, the same input must produce the same bytes.
//
// This is what makes a before/after comparison of real manifests meaningful:
// publishTime was previously read from time.Now() deep inside Generate, so two
// runs never matched and any byte-level regression check had to strip it first.
func TestGenerateIsReproducibleWithPinnedClock(t *testing.T) {
	p := testMPD(t)
	cfg := GeneratorConfig{
		GatewayBaseURL: "https://gw.example.com",
		ChannelID:      "ch1",
		Now:            time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC),
	}

	first, err := Generate(p, index.NewChannelIndex().Snapshot(), cfg)
	if err != nil {
		t.Fatalf("first generate: %v", err)
	}
	second, err := Generate(p, index.NewChannelIndex().Snapshot(), cfg)
	if err != nil {
		t.Fatalf("second generate: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("two generations from identical input differ:\nfirst:  %s\nsecond: %s",
			truncateXML(first), truncateXML(second))
	}
}

// A zero Now must keep the production behavior of stamping the current time, so
// that adding the field changed nothing for existing callers.
func TestGenerateZeroNowUsesWallClock(t *testing.T) {
	p := testMPD(t)
	p.Type = imdp.PresentationDynamic // publishTime is only emitted for dynamic

	before := time.Now().UTC().Add(-time.Second)
	out, err := Generate(p, index.NewChannelIndex().Snapshot(), GeneratorConfig{
		GatewayBaseURL: "https://gw.example.com",
		ChannelID:      "ch1",
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	stamp := publishTimeOf(t, out)
	if stamp.Before(before) || stamp.After(after) {
		t.Errorf("publishTime %v is outside [%v, %v]; a zero cfg.Now must mean 'now'",
			stamp, before, after)
	}
}

func publishTimeOf(t *testing.T, mpd []byte) time.Time {
	t.Helper()
	got := attrValue(string(mpd), "publishTime")
	if got == "" {
		t.Fatalf("no publishTime in output:\n%s", truncateXML(mpd))
	}
	// xsd.DateTime renders without a timezone designator, so the value is
	// unzoned text that the generator produced from a UTC time. Parse it back
	// the same way rather than assuming RFC3339.
	stamp, err := time.Parse("2006-01-02T15:04:05.999999999", got)
	if err != nil {
		t.Fatalf("publishTime %q not parseable: %v", got, err)
	}
	return stamp.UTC()
}

func truncateXML(b []byte) string {
	const n = 300
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
