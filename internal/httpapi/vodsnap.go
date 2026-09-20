package httpapi

// VOD snapshotting: the on-disk manifest a transitioned channel is restored
// from, and the index selection that makes a VOD channel serve its full
// history rather than the live window.

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/hlsout"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/manifest"
)

// SaveVODManifest generates the final VOD MPD and writes it to
// {dataDir}/{channelID}/manifest.mpd so it can be used as a static source.
//
// It is exported because main.go registers it as the manager's OnVODReady
// callback. Nothing in this package calls it.
func (s *Server) SaveVODManifest(rt *ch.Runtime) {
	sv := rt.Serving()
	p := sv.ParsedMPD
	// The full-history index specifically, not whichever index is active: this
	// callback exists to snapshot the completed VOD, and a channel that has not
	// built one has nothing to save.
	vodIdx := rt.VODIndex()
	if vodIdx == nil {
		return
	}

	// An HLS channel has no parsed MPD and so has never had a manifest.mpd
	// snapshot; it still has segments on disk, and the local playlists are for
	// exactly that. Handled before the nil check below so the DASH-only path
	// keeps its existing shape.
	if sv.IsHLSSource {
		dir := filepath.Join(s.cfg.Store.DataDir, rt.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			slog.Warn("local playlist: mkdir failed", "channel", rt.ID, "err", err)
			return
		}
		writeLocalPlaylists(rt, dir, vodIdx.Snapshot())
		return
	}
	if p == nil {
		return
	}
	baseURL := s.cfg.Server.BaseURL
	chCfg := sv.Config
	cfg := manifest.GeneratorConfig{
		GatewayBaseURL:     baseURL,
		ChannelID:          rt.ID,
		LicenseURL:         licenseProxyURL(baseURL, rt.ID, "playready", chCfg.PlayReadyEnabled()),
		WidevineLicenseURL: licenseProxyURL(baseURL, rt.ID, "widevine", chCfg.WidevineEnabled()),
		DisablePlayReady:   !chCfg.PlayReadyEnabled(),
		DisableWidevine:    !chCfg.WidevineEnabled(),
		VODMode:            true,
		FinalDuration:      sv.FinalDuration,
	}
	data, err := manifest.Generate(p, vodIdx.Snapshot(), cfg)
	if err != nil {
		slog.Warn("saveVODManifest: generate failed", "channel", rt.ID, "err", err)
		return
	}
	dir := filepath.Join(s.cfg.Store.DataDir, rt.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("saveVODManifest: mkdir failed", "channel", rt.ID, "err", err)
		return
	}
	dst := filepath.Join(dir, "manifest.mpd")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		slog.Warn("saveVODManifest: write failed", "channel", rt.ID, "err", err)
		return
	}
	slog.Info("VOD manifest saved", "channel", rt.ID, "path", dst)

	// The local manifest addresses segments by their path on disk rather than
	// by the gateway's URL shape, so the finished asset can be read straight
	// out of this directory. It is an extra: a failure here must not cost the
	// channel the manifest it is restored from, so it only warns.
	writeLocalManifest(rt, dir, vodIdx.Snapshot())

	// Save the upstream MPD so it can be restored on restart without re-fetching.
	if raw := rt.ParsedMPDBytes(); len(raw) > 0 {
		upDst := filepath.Join(dir, "upstream.mpd")
		if err := os.WriteFile(upDst, raw, 0o644); err != nil {
			slog.Warn("saveVODManifest: upstream write failed", "channel", rt.ID, "err", err)
		}
	}
}

func (s *Server) savedVODManifestPath(channelID string) string {
	path := filepath.Join(s.cfg.Store.DataDir, channelID, "manifest.mpd")
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// writeLocalManifest renders local.mpd, the filesystem-relative sibling of the
// served manifest, for a DASH source. An HLS source gets local.m3u8 and one
// playlist per representation instead — the output follows the source, as
// everything else in this gateway does.
func writeLocalManifest(rt *ch.Runtime, dir string, snap index.Snapshot) {
	data, err := manifest.GenerateLocalMPD(rt.Serving().ParsedMPD, snap, dir)
	if err != nil {
		slog.Warn("local manifest: generate failed", "channel", rt.ID, "err", err)
		return
	}
	dst := filepath.Join(dir, manifest.LocalManifestName)
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		slog.Warn("local manifest: write failed", "channel", rt.ID, "err", err)
		return
	}
	slog.Info("local manifest saved", "channel", rt.ID, "path", dst)
}

func writeLocalPlaylists(rt *ch.Runtime, dir string, snap index.Snapshot) {
	// Descriptive attributes for the master come from the HLS watcher, which is
	// the only thing that knows a variant's bandwidth and codecs; the segments
	// come from the index. A variant the watcher no longer reports still gets a
	// playlist — what is on disk is what this manifest is about.
	byRep := map[string]hlsdesc.Variant{}
	if rt.HLS != nil {
		for _, v := range rt.HLS.Variants() {
			byRep[v.RepID] = v
		}
	}

	var rends []hlsout.LocalRendition
	snap.ForEachRep(func(ref index.RepRef, published []index.SegmentState) {
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
		slog.Warn("local playlist: no published segments", "channel", rt.ID)
		return
	}
	// ForEachRep walks maps, so fix an order before the master and the media
	// playlists are numbered against each other.
	sort.Slice(rends, func(i, j int) bool {
		a, b := rends[i].Ref, rends[j].Ref
		if a.PeriodID != b.PeriodID {
			return a.PeriodID < b.PeriodID
		}
		if a.ASID != b.ASID {
			return a.ASID < b.ASID
		}
		return a.RepID < b.RepID
	})

	for i, r := range rends {
		data, err := hlsout.LocalMedia(r.Segments, dir)
		if err != nil {
			slog.Warn("local playlist: generate failed",
				"channel", rt.ID, "rep", r.Ref.RepID, "err", err)
			return
		}
		if err := os.WriteFile(filepath.Join(dir, hlsout.LocalMediaName(i)), data, 0o644); err != nil {
			slog.Warn("local playlist: write failed",
				"channel", rt.ID, "rep", r.Ref.RepID, "err", err)
			return
		}
	}
	dst := filepath.Join(dir, hlsout.LocalMasterName)
	if err := os.WriteFile(dst, hlsout.LocalMaster(rends), 0o644); err != nil {
		slog.Warn("local playlist: master write failed", "channel", rt.ID, "err", err)
		return
	}
	slog.Info("local playlist saved", "channel", rt.ID, "path", dst, "renditions", len(rends))
}
