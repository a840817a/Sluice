// Package hlsdesc describes the tracks of an HLS presentation: the attributes
// that live only in a multivariant playlist and cannot be recovered from the
// segment index or the on-disk layout.
//
// It is dependency-free on purpose. The ingest side discovers these values and
// the output side renders them, and neither may import the other: hlsingest ->
// hlsout would make ingestion depend on output, hlsout -> hlsingest would make
// the renderer depend on the poller. A neutral leaf is the only placement that
// keeps the import graph acyclic, and it is why these three structs previously
// existed twice, field for field, with a copy loop bridging them.
//
// These types describe what the gateway PUBLISHES. They are deliberately not
// hlssrc's parse shapes, which carry the upstream URI and no RepID because
// RepID is assigned here rather than upstream.
package hlsdesc

// Variant is one video rendition offered in the multivariant playlist.
//
// The json tags exist for hlsingest's on-disk source state, which embeds this
// type; nothing else marshals it.
type Variant struct {
	// RepID is the gateway's own identifier for this representation, assigned
	// at discovery ("v0", "v1", ...). Every gateway URL and index lookup keys
	// on it.
	RepID            string  `json:"rep_id"`
	Bandwidth        uint64  `json:"bandwidth,omitempty"`
	AverageBandwidth uint64  `json:"average_bandwidth,omitempty"`
	Codecs           string  `json:"codecs,omitempty"`
	Width            int     `json:"width,omitempty"`
	Height           int     `json:"height,omitempty"`
	FrameRate        float64 `json:"frame_rate,omitempty"`
	// AudioGroup binds this variant to a demuxed audio rendition group via
	// EXT-X-STREAM-INF AUDIO. Empty when audio is muxed into the variant.
	AudioGroup string `json:"audio_group,omitempty"`
	// SubtitleGroup binds this variant to a subtitle group via SUBTITLES.
	SubtitleGroup string `json:"subtitle_group,omitempty"`
}

// Rendition is one demuxed audio track, emitted as EXT-X-MEDIA:TYPE=AUDIO.
type Rendition struct {
	RepID    string
	GroupID  string
	Name     string
	Language string
	Default  bool
	Channels string
}

// Subtitle is one demuxed WebVTT track, emitted as EXT-X-MEDIA:TYPE=SUBTITLES.
type Subtitle struct {
	RepID    string
	GroupID  string
	Name     string
	Language string
	Default  bool
	Forced   bool
}
