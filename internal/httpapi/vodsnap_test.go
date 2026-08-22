package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
)

// A VOD transition snapshots the final manifest to disk so a restarted gateway
// can serve the channel without reaching upstream again. That snapshot happens
// through a callback the manager invokes; nothing in the serving path calls it.
//
// What this test covers: that SaveVODManifest is a working OnVODReady callback —
// that a real transition invokes it and that it writes a restorable manifest.
// It is wired here the same way cmd/gateway wires it.
//
// What it does NOT cover: the gateway's registration line itself. This test
// registers the callback, so deleting that line would leave this test green.
// The registration is covered separately by
// cmd/gateway.TestNewGatewayRegistersVODSnapshotCallback, which seeds
// channels.json and lets the composed gateway wire itself.
func TestVODTransitionSnapshotsManifestToDisk(t *testing.T) {
	origin := startClearDASHOrigin(t)

	dataDir := t.TempDir()
	cfg := config.Config{
		Server: config.ServerConfig{BaseURL: "http://gw.test"},
		Store:  config.StoreConfig{DataDir: dataDir},
		Worker: config.WorkerConfig{FetchWorkers: 2, MaxRetriesStatic: 2},
		Window: config.WindowConfig{Depth: time.Hour},
	}

	mgr := ch.NewManager(cfg)
	srv := NewServer(cfg, mgr)
	// The line under test, copied from cmd/gateway/main.go.
	mgr.SetOnVODReady(srv.SaveVODManifest)
	t.Cleanup(func() { _ = mgr.Stop("vodsnap") })

	if _, err := mgr.Start(context.Background(), "vodsnap", ch.Config{
		MPDURL:              origin.URL + "/manifest.mpd",
		Enabled:             true,
		EnableVODTransition: true,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	// The upstream MPD is static, so the channel ingests everything and
	// transitions to VOD on its own once the broker drains.
	manifestPath := filepath.Join(dataDir, "vodsnap", "manifest.mpd")
	upstreamPath := filepath.Join(dataDir, "vodsnap", "upstream.mpd")

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(manifestPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no VOD manifest at %s after 30s (channel mode %v). "+
				"If ingest worked but this file is missing, the OnVODReady callback "+
				"is not registered — see cmd/gateway/main.go.",
				manifestPath, mgr.Get("vodsnap").Mode())
		}
		time.Sleep(100 * time.Millisecond)
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read saved manifest: %v", err)
	}
	// A static manifest is what a restart needs; a dynamic one would send the
	// restored channel looking for a live edge that no longer exists.
	if got := string(data); !strings.Contains(got, `type="static"`) {
		t.Errorf("saved manifest is not static:\n%s", truncate(got, 400))
	}
	if !strings.Contains(string(data), "v1/channels/vodsnap/segments/") {
		t.Errorf("saved manifest does not point at this gateway:\n%s", truncate(string(data), 400))
	}

	// The raw upstream MPD is saved beside it so the channel can be restored
	// without re-fetching.
	if _, err := os.Stat(upstreamPath); err != nil {
		t.Errorf("upstream.mpd not saved alongside manifest.mpd: %v", err)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
