package index

import "testing"

// Commit reports whether the segment was new and how many segments it made
// visible, under the policy the index was built with. An audio AdaptationSet
// that has no segment yet holds the A/V barrier; PublishImmediately ignores
// it, as an HLS track must not wait for its peer.
func TestCommitPublishesUnderTheIndexPolicy(t *testing.T) {
	video := RepRef{PeriodID: "p", ASID: "0", RepID: "v0", MediaType: MediaVideo}
	audio := RepRef{PeriodID: "p", ASID: "1", RepID: "a0", MediaType: MediaAudio}
	seg := SegmentState{SegNo: 1, StartPTS: 0, EndPTS: 90000, Timescale: 90000}

	t.Run("A/V barrier", func(t *testing.T) {
		ci := NewChannelIndex(PublishAVBarrier)
		ci.Period("p").AS("1").SetMediaType(MediaAudio)
		if got := ci.Commit(video, 8, seg); !got.New || got.Published != 0 {
			t.Errorf("video before its audio = %+v, want new and nothing published", got)
		}
		if got := ci.Commit(video, 8, seg); got.New {
			t.Errorf("recommitting segment 1 = %+v, want it reported as not new", got)
		}
		if got := ci.Commit(audio, 8, seg); !got.New || got.Published != 2 {
			t.Errorf("the matching audio = %+v, want new and both segments published", got)
		}
	})
	t.Run("immediately", func(t *testing.T) {
		ci := NewChannelIndex(PublishImmediately)
		ci.Period("p").AS("1").SetMediaType(MediaAudio)
		if got := ci.Commit(video, 8, seg); !got.New || got.Published != 1 {
			t.Errorf("video before its audio = %+v, want new and published", got)
		}
		if got := ci.Commit(video, 8, seg); got.New {
			t.Errorf("recommitting segment 1 = %+v, want it reported as not new", got)
		}
	})
}
