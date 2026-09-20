package channel

import (
	"testing"

	"github.com/a840817a/sluice/internal/progress"
)

// The broker going idle is not evidence that the ingest is complete: tasks shed
// under load empty the queue exactly as tasks fetched do. These are the cases
// that decide whether a finished static ingest gets called out as partial.
func TestStaticIngestShortfall(t *testing.T) {
	tests := []struct {
		name    string
		snap    progress.Snapshot
		dropped uint64
		want    bool
	}{
		{
			name: "complete",
			snap: progress.Snapshot{SegmentsStored: 32490, ExpectedSegments: 32490, TotalKnown: true},
			want: false,
		},
		{
			name: "the reported case: queue shed most of the asset",
			snap: progress.Snapshot{SegmentsStored: 4125, ExpectedSegments: 32490, TotalKnown: true},
			// 4125 + 28365 = 32490
			dropped: 28365,
			want:    true,
		},
		{
			name:    "drops with the count still reached are still a shortfall",
			snap:    progress.Snapshot{SegmentsStored: 32490, ExpectedSegments: 32490, TotalKnown: true},
			dropped: 7,
			want:    true,
		},
		{
			name: "a live stream states no total, so there is nothing to be short of",
			snap: progress.Snapshot{SegmentsStored: 900},
			want: false,
		},
		{
			name: "a capped enumeration knows its total is a floor",
			snap: progress.Snapshot{SegmentsStored: 100_000, ExpectedSegments: 100_000, TotalKnown: true, TotalCapped: true},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, stored, expected := staticIngestShortfall(tc.snap, tc.dropped)
			if got != tc.want {
				t.Errorf("staticIngestShortfall() = %v (stored %d, expected %d), want %v",
					got, stored, expected, tc.want)
			}
		})
	}
}

// A paced producer does not return from the source until the whole asset is
// queued, so "the source returned" is no longer the moment a static upstream
// becomes known. These are the cases that decide whether the channel is shown
// as ingesting a finished asset while it is still downloading it.
func TestStaticIngestVisible(t *testing.T) {
	tests := []struct {
		name           string
		upstreamType   string
		autoVODAllowed bool
		want           bool
	}{
		{"static upstream, ordinary channel", "static", true, true},
		{"live upstream", "dynamic", true, false},
		{"upstream not seen yet", "", true, false},
		{"static, but auto-VOD suppressed by a restore", "static", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := staticIngestVisible(tc.upstreamType, tc.autoVODAllowed); got != tc.want {
				t.Errorf("staticIngestVisible(%q, %v) = %v, want %v",
					tc.upstreamType, tc.autoVODAllowed, got, tc.want)
			}
		})
	}
}
