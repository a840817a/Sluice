package channel

// VOD artifacts are the files that make a completed channel usable after the
// gateway restarts: the served DASH manifest, the raw origin MPD it was
// generated from, and optional filesystem-relative playlists. They belong to
// the channel lifecycle because their inputs are the final runtime state and
// their consumer is VOD restore, not HTTP request handling.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/a840817a/sluice/internal/gwurl"
	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/hlsout"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/manifest"
	imdp "github.com/a840817a/sluice/internal/mpd"
	"github.com/a840817a/sluice/internal/store"
)

const (
	vodManifestFile = "manifest.mpd"
	vodUpstreamFile = "upstream.mpd"
)

// vodArtifacts is a single capture of all state needed to write a completed
// channel. Capturing before any I/O prevents a long artifact write from
// combining a later hot configuration update with the served manifest.
type vodArtifacts struct {
	id       string
	dir      string
	baseURL  string
	serving  Serving
	snapshot index.Snapshot
	rawMPD   []byte
	variants []hlsdesc.Variant
}

// captureVODArtifacts freezes the candidate index the VOD transition just
// rebuilt.
// The runtime has not been published as VOD yet, so it deliberately does not
// consult rt.VODIndex: doing so would make durable artifacts depend on an
// already-visible VOD state.
func (m *Manager) captureVODArtifacts(rt *Runtime, candidate *index.ChannelIndex) (vodArtifacts, error) {
	if candidate == nil {
		return vodArtifacts{}, errors.New("missing VOD index")
	}
	sv := rt.Serving()
	sv.Mode = ModeVOD
	sv.Index = candidate

	a := vodArtifacts{
		id:       rt.ID,
		dir:      m.vodArtifactDir(rt.ID),
		baseURL:  m.gwCfg.Server.BaseURL,
		serving:  sv,
		snapshot: candidate.Snapshot(),
		rawMPD:   append([]byte(nil), rt.ParsedMPDBytes()...),
	}
	if sv.IsHLSSource {
		a.variants = sv.Presentation.Variants
	}
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
	if a.serving.IsHLSSource {
		if err := os.MkdirAll(a.dir, 0o755); err != nil {
			slog.Warn("local playlist: mkdir failed", "channel", a.id, "err", err)
			return
		}
		a.writeLocalPlaylists()
		return
	}
	a.writeLocalManifest()
}

func generateServedVODManifest(a vodArtifacts) ([]byte, error) {
	cfg := manifest.GeneratorConfig{
		GatewayBaseURL:     a.baseURL,
		ChannelID:          a.id,
		LicenseURL:         vodLicenseURL(a.baseURL, a.id, "playready", a.serving.Config.PlayReadyEnabled()),
		WidevineLicenseURL: vodLicenseURL(a.baseURL, a.id, "widevine", a.serving.Config.WidevineEnabled()),
		DisablePlayReady:   !a.serving.Config.PlayReadyEnabled(),
		DisableWidevine:    !a.serving.Config.WidevineEnabled(),
		VODMode:            true,
		FinalDuration:      a.serving.FinalDuration,
	}
	return manifest.Generate(a.serving.ParsedMPD, a.snapshot, cfg)
}

func vodLicenseURL(baseURL, channelID, drmType string, enabled bool) string {
	if !enabled {
		return ""
	}
	return gwurl.LicenseURL(baseURL, channelID, drmType)
}

func (a vodArtifacts) writeLocalManifest() {
	data, err := manifest.GenerateLocalMPD(a.serving.ParsedMPD, a.snapshot, a.dir)
	if err != nil {
		slog.Warn("local manifest: generate failed", "channel", a.id, "err", err)
		return
	}
	path := filepath.Join(a.dir, manifest.LocalManifestName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		slog.Warn("local manifest: write failed", "channel", a.id, "err", err)
		return
	}
	slog.Info("local manifest saved", "channel", a.id, "path", path)
}

func (a vodArtifacts) writeLocalPlaylists() {
	byRep := make(map[string]hlsdesc.Variant, len(a.variants))
	for _, v := range a.variants {
		byRep[v.RepID] = v
	}

	var rends []hlsout.LocalRendition
	a.snapshot.ForEachRep(func(ref index.RepRef, published []index.SegmentState) {
		if len(published) == 0 {
			return
		}
		r := hlsout.LocalRendition{Ref: ref, Segments: published}
		if v, ok := byRep[ref.RepID]; ok {
			r.Bandwidth, r.Codecs, r.Width, r.Height = v.Bandwidth, v.Codecs, v.Width, v.Height
		}
		rends = append(rends, r)
	})
	if len(rends) == 0 {
		slog.Warn("local playlist: no published segments", "channel", a.id)
		return
	}
	sort.Slice(rends, func(i, j int) bool {
		x, y := rends[i].Ref, rends[j].Ref
		if x.PeriodID != y.PeriodID {
			return x.PeriodID < y.PeriodID
		}
		if x.ASID != y.ASID {
			return x.ASID < y.ASID
		}
		return x.RepID < y.RepID
	})

	// Media playlists must exist before the master points at them.
	for i, r := range rends {
		data, err := hlsout.LocalMedia(r.Segments, a.dir)
		if err != nil {
			slog.Warn("local playlist: generate failed", "channel", a.id, "rep", r.Ref.RepID, "err", err)
			return
		}
		if err := os.WriteFile(filepath.Join(a.dir, hlsout.LocalMediaName(i)), data, 0o644); err != nil {
			slog.Warn("local playlist: write failed", "channel", a.id, "rep", r.Ref.RepID, "err", err)
			return
		}
	}
	path := filepath.Join(a.dir, hlsout.LocalMasterName)
	if err := os.WriteFile(path, hlsout.LocalMaster(rends), 0o644); err != nil {
		slog.Warn("local playlist: master write failed", "channel", a.id, "err", err)
		return
	}
	slog.Info("local playlist saved", "channel", a.id, "path", path, "renditions", len(rends))
}

func (m *Manager) vodArtifactDir(channelID string) string {
	return filepath.Join(m.gwCfg.Store.DataDir, channelID)
}

func (m *Manager) vodArtifactPath(channelID, file string) string {
	return filepath.Join(m.vodArtifactDir(channelID), file)
}

// SavedVODManifestPath returns the persisted served manifest when it exists.
// HTTP uses this only as a fallback when an old VOD record has no parseable
// origin MPD; lifecycle code owns both the name and existence check.
func (m *Manager) SavedVODManifestPath(channelID string) string {
	path := m.vodArtifactPath(channelID, vodManifestFile)
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

func (m *Manager) restoreVODOriginMPD(rt *Runtime) {
	if rt.watcher == nil {
		return
	}
	raw, err := os.ReadFile(m.vodArtifactPath(rt.ID, vodUpstreamFile))
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
}
