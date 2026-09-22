package channel

// VOD artifacts are the files that make a completed channel usable after the
// gateway restarts: the served DASH manifest, the raw upstream MPD it was
// generated from, and optional filesystem-relative playlists. They belong to
// the channel lifecycle because their inputs are the final runtime state and
// their consumer is VOD restore, not HTTP request handling.

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/a840817a/sluice/internal/gwurl"
	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/hlsout"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/manifest"
	imdp "github.com/a840817a/sluice/internal/mpd"
)

const (
	vodManifestFile = "manifest.mpd"
	vodUpstreamFile = "upstream.mpd"
)

// vodArtifacts is a single capture of all state needed to write a completed
// channel. Capturing before any I/O prevents a long local-artifact write from
// combining a later index or watcher update with the served manifest.
type vodArtifacts struct {
	id       string
	dir      string
	serving  Serving
	snapshot index.Snapshot
	rawMPD   []byte
	variants []hlsdesc.Variant
}

func (m *Manager) captureVODArtifacts(rt *Runtime) (vodArtifacts, bool) {
	sv := rt.Serving()
	vodIndex := rt.VODIndex()
	if vodIndex == nil {
		return vodArtifacts{}, false
	}

	a := vodArtifacts{
		id:       rt.ID,
		dir:      m.vodArtifactDir(rt.ID),
		serving:  sv,
		snapshot: vodIndex.Snapshot(),
		rawMPD:   append([]byte(nil), rt.ParsedMPDBytes()...),
	}
	if rt.HLS != nil {
		a.variants = rt.HLS.Variants()
	}
	return a, true
}

// SaveVODArtifacts persists the final VOD state after a successful transition.
// It is suitable as an OnVODReady callback. The primary served MPD is written
// before the optional local artifact; failures generating local output never
// prevent the saved upstream MPD from being available for an offline restart.
func (m *Manager) SaveVODArtifacts(rt *Runtime) {
	a, ok := m.captureVODArtifacts(rt)
	if !ok {
		return
	}

	if a.serving.IsHLSSource {
		if err := os.MkdirAll(a.dir, 0o755); err != nil {
			slog.Warn("local playlist: mkdir failed", "channel", a.id, "err", err)
			return
		}
		a.writeLocalPlaylists()
		return
	}
	if a.serving.ParsedMPD == nil {
		return
	}
	if err := os.MkdirAll(a.dir, 0o755); err != nil {
		slog.Warn("save VOD artifacts: mkdir failed", "channel", a.id, "err", err)
		return
	}

	if data, err := m.generateServedVODManifest(a); err != nil {
		slog.Warn("save VOD artifacts: generate manifest failed", "channel", a.id, "err", err)
		return
	} else if err := os.WriteFile(filepath.Join(a.dir, vodManifestFile), data, 0o644); err != nil {
		slog.Warn("save VOD artifacts: write manifest failed", "channel", a.id, "err", err)
		return
	} else {
		slog.Info("VOD manifest saved", "channel", a.id, "path", filepath.Join(a.dir, vodManifestFile))
	}

	// The local manifest is useful outside the gateway, but the raw upstream
	// MPD is what lets this channel recover its served manifest after restart.
	// Keep those writes independent.
	a.writeLocalManifest()
	a.writeVODUpstreamMPD()
}

func (m *Manager) generateServedVODManifest(a vodArtifacts) ([]byte, error) {
	cfg := manifest.GeneratorConfig{
		GatewayBaseURL:     m.gwCfg.Server.BaseURL,
		ChannelID:          a.id,
		LicenseURL:         vodLicenseURL(m.gwCfg.Server.BaseURL, a.id, "playready", a.serving.Config.PlayReadyEnabled()),
		WidevineLicenseURL: vodLicenseURL(m.gwCfg.Server.BaseURL, a.id, "widevine", a.serving.Config.WidevineEnabled()),
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

func (a vodArtifacts) writeVODUpstreamMPD() {
	if len(a.rawMPD) == 0 {
		return
	}
	path := filepath.Join(a.dir, vodUpstreamFile)
	if err := os.WriteFile(path, a.rawMPD, 0o644); err != nil {
		slog.Warn("save VOD artifacts: upstream write failed", "channel", a.id, "err", err)
	}
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
// upstream MPD; lifecycle code owns both the name and existence check.
func (m *Manager) SavedVODManifestPath(channelID string) string {
	path := m.vodArtifactPath(channelID, vodManifestFile)
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

func (m *Manager) restoreVODUpstreamMPD(rt *Runtime) {
	if rt.Watcher == nil {
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
	rt.Watcher.SetParsedMPD(p, raw)
}
