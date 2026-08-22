package hlsingest

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/store"
)

// SourceStateFileName holds the discovered description of an HLS upstream.
const SourceStateFileName = "hls-source.json"

// SourceStatePath returns where a channel's source description is kept.
func SourceStatePath(dataDir, channelID string) string {
	return filepath.Join(dataDir, channelID, SourceStateFileName)
}

// sourceState is what a restart needs to serve playlists without re-contacting
// the upstream.
//
// The variant attributes (rep IDs, bandwidth, codecs, resolution) exist only in
// the upstream master playlist, not in the segment index or on-disk layout. A
// channel restored in VOD mode never runs the watcher, so without this file it
// would come back up unable to name its own representations. This is the HLS
// counterpart of the upstream.mpd snapshot the DASH path keeps.
type sourceState struct {
	// Version guards against reading a file written by an older layout. Unknown
	// JSON keys are silently ignored, so without this an old file would load as
	// zero values and the channel would come back up serving 404s for every
	// playlist instead of re-running discovery.
	Version             int                `json:"version"`
	Variants            []persistedVariant `json:"variants"`
	IndependentSegments bool               `json:"independent_segments"`
	FMP4                bool               `json:"fmp4"`
	KeyURIs             map[string]string  `json:"key_uris,omitempty"`
}

// sourceStateVersion is the layout this build reads and writes. Bump it whenever
// a field changes meaning; loadState discards anything else.
const sourceStateVersion = 2

// persistedVariant is the on-disk form of a poll target. It embeds the shared
// descriptor rather than restating its nine fields, so a field added to
// hlsdesc.Variant cannot be silently left out of the persisted state.
//
// The rendition half is not embedded: hlsdesc.Rendition and hlsdesc.Subtitle
// both carry RepID/GroupID/Name/Language/Default, so embedding either would make
// those selectors ambiguous. Those fields stay flat and hand-mapped.
type persistedVariant struct {
	hlsdesc.Variant
	URL       string `json:"url"`
	ASID      string `json:"as_id,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	// Demuxed rendition fields.
	Rendition bool   `json:"rendition,omitempty"`
	Kind      int    `json:"kind,omitempty"`
	GroupID   string `json:"group_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Language  string `json:"language,omitempty"`
	Default   bool   `json:"default,omitempty"`
	Forced    bool   `json:"forced,omitempty"`
	Channels  string `json:"channels,omitempty"`
}

// saveState writes the current source description. Best-effort: failing to
// persist only degrades restart recovery, never live serving.
func (w *Watcher) saveState() {
	if w.statePath == "" {
		return
	}

	w.mu.Lock()
	st := sourceState{
		Version:             sourceStateVersion,
		IndependentSegments: w.independentSegments,
		FMP4:                w.fmp4,
		KeyURIs:             make(map[string]string, len(w.keyURIs)),
	}
	for _, v := range w.variants {
		st.Variants = append(st.Variants, persistedVariant{
			Variant:   v.Variant,
			URL:       v.url,
			ASID:      v.asID,
			MediaType: v.mediaType,
			Rendition: v.rendition,
			Kind:      int(v.kind),
			GroupID:   v.groupID,
			Name:      v.name,
			Language:  v.language,
			Default:   v.isDefault,
			Forced:    v.forced,
			Channels:  v.channels,
		})
	}
	for id, uri := range w.keyURIs {
		st.KeyURIs[id] = uri
	}
	w.mu.Unlock()

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		slog.Warn("HLS source state marshal failed", "channel", w.channelID, "err", err)
		return
	}
	if err := store.Write(w.statePath, data); err != nil {
		slog.Warn("HLS source state write failed", "channel", w.channelID, "err", err)
	}
}

// loadState restores a previously discovered source description so playlists
// can be served before (or entirely without) a fresh discovery pass.
func (w *Watcher) loadState() {
	if w.statePath == "" {
		return
	}
	data, err := os.ReadFile(w.statePath)
	if err != nil {
		return // no previous state; discovery will produce it
	}
	var st sourceState
	if err := json.Unmarshal(data, &st); err != nil {
		slog.Warn("HLS source state parse failed", "channel", w.channelID, "err", err)
		return
	}
	if st.Version != sourceStateVersion {
		// Discarding is the safe response: discovery will rebuild the state on
		// the next poll. Loading it anyway would half-populate the variants and
		// serve a channel that names representations it cannot find.
		slog.Warn("HLS source state discarded: unsupported version",
			"channel", w.channelID, "got", st.Version, "want", sourceStateVersion)
		return
	}

	variants := make([]variant, 0, len(st.Variants))
	for _, v := range st.Variants {
		variants = append(variants, variant{
			Variant:   v.Variant,
			url:       v.URL,
			asID:      v.ASID,
			mediaType: v.MediaType,
			rendition: v.Rendition,
			kind:      renditionKind(v.Kind),
			groupID:   v.GroupID,
			name:      v.Name,
			language:  v.Language,
			isDefault: v.Default,
			forced:    v.Forced,
			channels:  v.Channels,
		})
	}

	w.mu.Lock()
	w.variants = variants
	w.independentSegments = st.IndependentSegments
	w.fmp4 = st.FMP4
	for id, uri := range st.KeyURIs {
		w.keyURIs[id] = uri
	}
	w.mu.Unlock()
}
