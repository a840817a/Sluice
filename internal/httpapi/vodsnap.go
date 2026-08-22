package httpapi

// VOD snapshotting: the on-disk manifest a transitioned channel is restored
// from, and the index selection that makes a VOD channel serve its full
// history rather than the live window.

import (
	"log/slog"
	"os"
	"path/filepath"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/manifest"
)

// SaveVODManifest generates the final VOD MPD and writes it to
// {dataDir}/{channelID}/manifest.mpd so it can be used as a static source.
//
// It is exported because main.go registers it as the manager's OnVODReady
// callback. Nothing in this package calls it.
func (s *Server) SaveVODManifest(rt *ch.Runtime) {
	p := rt.ParsedMPD()
	if p == nil || rt.VODIndex() == nil {
		return
	}
	baseURL := s.cfg.Server.BaseURL
	chCfg := rt.Config()
	cfg := manifest.GeneratorConfig{
		GatewayBaseURL:     baseURL,
		ChannelID:          rt.ID,
		LicenseURL:         licenseProxyURL(baseURL, rt.ID, "playready", chCfg.PlayReadyEnabled()),
		WidevineLicenseURL: licenseProxyURL(baseURL, rt.ID, "widevine", chCfg.WidevineEnabled()),
		DisablePlayReady:   !chCfg.PlayReadyEnabled(),
		DisableWidevine:    !chCfg.WidevineEnabled(),
		VODMode:            true,
		FinalDuration:      rt.FinalDuration(),
	}
	data, err := manifest.Generate(p, rt.VODIndex().Snapshot(), cfg)
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
