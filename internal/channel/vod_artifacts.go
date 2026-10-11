package channel

// VOD artifacts are the files that make a completed channel usable after the
// gateway restarts: the served DASH manifest, the raw origin MPD it was
// generated from, and optional filesystem-relative playlists. They belong to
// the channel lifecycle because their inputs are the final runtime state and
// their consumer is VOD restore, not HTTP request handling.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/a840817a/sluice/internal/hlskey"
	"github.com/a840817a/sluice/internal/index"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/presentation"
	"github.com/a840817a/sluice/internal/store"
)

const (
	vodManifestFile = "manifest.mpd"
	vodUpstreamFile = "upstream.mpd"
)

// vodAsset owns the files that make a completed channel restartable without
// its origin, and nothing else does: what the VOD transition writes once
// ingest has stopped, and what a VOD restore reads back. Media written during
// live ingest (segments, inits, sidecars, period origin MPDs) belongs to the
// pipeline and the source; vodAsset only makes it durable and complete.
//
// The transition calls it in this order, and owns the order itself:
// checkpointMedia, then (after the rebuild) checkpointSource and capture,
// whose persistDASHManifests runs before the VOD is published and whose
// writeOptional after. A restart in VOD calls restore.
type vodAsset struct {
	dataDir string
	render  *presentation.Generator
}

// dir is the channel's directory, where its VOD files live.
func (v vodAsset) dir(channelID string) string { return store.ChannelDir(v.dataDir, channelID) }

func (v vodAsset) path(channelID, file string) string {
	return filepath.Join(v.dir(channelID), file)
}

// checkpointSource makes the source's own restart state durable: for HLS its
// variant description and, in passthrough, the keys the VOD still needs. It
// may fetch keys over the network, so the transition runs it before configMu
// (a hot edit must not wait on an unreachable key server), and it is bounded
// by keyCheckpointTimeout. Its failures are warnings: the completed VOD is
// served from the in-memory index either way, and only a later restart
// depends on these files.
func (v vodAsset) checkpointSource(rt *Runtime, snapshot index.Snapshot) []string {
	if rt.Source == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(lifecycleContext(rt), keyCheckpointTimeout)
	defer cancel()
	return rt.Source.Checkpoint(ctx, snapshot)
}

// vodArtifacts is a single capture of all state needed to write a completed
// channel. Capturing before any I/O prevents a long artifact write from
// combining a later hot configuration update with the served manifest.
type vodArtifacts struct {
	id       string
	dir      string
	render   *presentation.Generator
	serving  Serving
	snapshot index.Snapshot
	rawMPD   []byte
	keys     *hlskey.Cache
	// originMPD is what the VOD serves its DASH manifests from, and
	// periodWarnings name the periods it had to leave out (vodOriginMPD).
	originMPD      *imdp.ParsedMPD
	periodWarnings []string
}

// capture freezes the candidate index the VOD transition just
// rebuilt.
// The runtime has not been published as VOD yet, so it deliberately does not
// consult rt.VODIndex: doing so would make durable artifacts depend on an
// already-visible VOD state.
func (v vodAsset) capture(rt *Runtime, candidate *index.ChannelIndex) (vodArtifacts, error) {
	if candidate == nil {
		return vodArtifacts{}, errors.New("missing VOD index")
	}
	sv := rt.servingAs(ModeVOD, candidate)
	snapshot := candidate.Snapshot()
	originMPD, periodWarnings := vodOriginMPD(sv.ParsedMPD, snapshot, v.dataDir, rt.ID)
	if originMPD != sv.ParsedMPD {
		sv.ParsedMPD = originMPD
		sv.Tracks = rt.tracks(originMPD)
	}

	a := vodArtifacts{
		id:             rt.ID,
		dir:            v.dir(rt.ID),
		render:         v.render,
		serving:        sv,
		snapshot:       snapshot,
		rawMPD:         append([]byte(nil), rt.ParsedMPDBytes()...),
		originMPD:      originMPD,
		periodWarnings: periodWarnings,
	}
	a.keys = rt.keys
	return a, nil
}

// keyCheckpointTimeout bounds all key fetches of one VOD transition. Each
// fetch has its own client timeout, but a slow key server answering just
// inside it could otherwise hold completion for that long per key.
var keyCheckpointTimeout = 60 * time.Second

// persistDASHManifests writes the DASH files a VOD restart needs: the served
// manifest and the raw origin MPD. A failure is returned as a warning, not
// an error, because a restart can still serve the VOD without either file: it
// falls back to the saved manifest when the origin snapshot is missing.
// Optional local artifacts are written separately, after publication.
func (a vodArtifacts) persistDASHManifests() []string {
	if a.serving.IsHLSSource {
		return nil
	}
	if a.serving.ParsedMPD == nil || len(a.rawMPD) == 0 {
		return []string{"save VOD artifacts: no final MPD captured"}
	}
	var warnings []string
	manifestData, err := generateServedVODManifest(a)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("generate served manifest: %v", err))
	} else if err := store.WriteDurable(filepath.Join(a.dir, vodManifestFile), manifestData, 0o644); err != nil {
		warnings = append(warnings, fmt.Sprintf("write served manifest: %v", err))
	}
	if err := store.WriteDurable(filepath.Join(a.dir, vodUpstreamFile), a.rawMPD, 0o644); err != nil {
		warnings = append(warnings, fmt.Sprintf("write upstream MPD: %v", err))
	}
	if len(warnings) == 0 {
		slog.Info("VOD artifacts saved", "channel", a.id, "dir", a.dir)
	}
	return warnings
}

// requiredKeyURIs returns the key material a passthrough HLS VOD still needs
// after its origin has gone away. The segment snapshot is the authority: a
// URI merely observed during discovery but absent from the completed asset is
// not required for its offline playback.
func requiredKeyURIs(snapshot index.Snapshot) []string {
	set := make(map[string]struct{})
	snapshot.ForEachRep(func(_ index.RepRef, segments []index.SegmentState) {
		for _, seg := range segments {
			if seg.KeyURI != "" {
				set[seg.KeyURI] = struct{}{}
			}
		}
	})
	uris := make([]string, 0, len(set))
	for uri := range set {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	return uris
}

// writeOptional produces filesystem-relative exports after required state has
// been committed. Its failures are deliberately contained: they cannot revoke
// a completed VOD that is already restartable through the gateway.
func (a vodArtifacts) writeOptional() {
	files, err := a.render.Local(a.serving.Channel, a.snapshot, a.dir, a.keys)
	if err != nil {
		slog.Warn("local presentation: generate failed", "channel", a.id, "err", err)
		return
	}
	for _, file := range files {
		if err := store.WriteDurable(filepath.Join(a.dir, file.Name), file.Data, file.Mode); err != nil {
			slog.Warn("local presentation: write failed", "channel", a.id, "file", file.Name, "err", err)
			return
		}
	}
	slog.Info("local presentation saved", "channel", a.id, "dir", a.dir)
}

func generateServedVODManifest(a vodArtifacts) ([]byte, error) {
	return a.render.DASH(a.serving.Channel, a.snapshot)
}

// SavedVODManifestPath returns the persisted served manifest when it exists.
// HTTP uses this only as a fallback when an old VOD record has no parseable
// origin MPD; lifecycle code owns both the name and existence check.
func (m *Manager) SavedVODManifestPath(channelID string) string {
	path := m.vod.path(channelID, vodManifestFile)
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// restore reads back what a restart in VOD needs that the index rebuild does
// not: a DASH channel's last origin MPD, with every stored period put back
// (vodOriginMPD).
func (v vodAsset) restore(rt *Runtime) {
	if rt.watcher == nil {
		return
	}
	raw, err := os.ReadFile(v.path(rt.ID, vodUpstreamFile))
	if err != nil {
		slog.Warn("VOD restore: upstream MPD unavailable; using saved manifest fallback", "channel", rt.ID, "err", err)
		return
	}
	p, err := imdp.Parse(raw)
	if err != nil {
		slog.Warn("VOD restore: upstream MPD parse failed", "channel", rt.ID, "err", err)
		return
	}
	rt.watcher.SetParsedMPD(p, raw)
	if vod := rt.VODIndex(); vod != nil {
		merged, warnings := vodOriginMPD(rt.watcher.ParsedMPD(), vod.Snapshot(), v.dataDir, rt.ID)
		for _, w := range warnings {
			slog.Warn("VOD restore: "+w, "channel", rt.ID)
		}
		rt.vodMPD.Store(merged)
	}
}
