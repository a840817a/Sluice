package httpapi

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
)

// The served manifest addresses segments the way the gateway serves them, which
// is nothing that exists on disk. The local manifest is the sibling that names
// the actual files, and the only thing worth asserting about it is that those
// files are there: well-formed XML pointing at paths that do not exist is the
// failure mode, not a lesser success.
func TestVODTransitionWritesResolvableLocalManifest(t *testing.T) {
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
	mgr.SetOnVODReady(srv.SaveVODManifest)
	t.Cleanup(func() { _ = mgr.Stop("localsnap") })

	if _, err := mgr.Start(context.Background(), "localsnap", ch.Config{
		MPDURL:  origin.URL + "/manifest.mpd",
		Enabled: true,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	channelDir := filepath.Join(dataDir, "localsnap")
	localPath := filepath.Join(channelDir, "local.mpd")

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(localPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no local manifest at %s after 30s (channel mode %v). "+
				"If manifest.mpd exists but this does not, SaveVODManifest is not "+
				"writing the local sibling.", localPath, mgr.Get("localsnap").Mode())
		}
		time.Sleep(100 * time.Millisecond)
	}

	data, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("read local manifest: %v", err)
	}
	if strings.Contains(string(data), "<BaseURL>") {
		t.Errorf("local manifest carries a BaseURL; paths must resolve against the file itself:\n%s", data)
	}

	var refs int
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		for _, a := range se.Attr {
			if a.Name.Local != "sourceURL" && a.Name.Local != "media" {
				continue
			}
			refs++
			if filepath.IsAbs(a.Value) || strings.HasPrefix(a.Value, "http") {
				t.Errorf("%q is not a relative path", a.Value)
				continue
			}
			if _, err := os.Stat(filepath.Join(channelDir, filepath.FromSlash(a.Value))); err != nil {
				t.Errorf("local manifest names %q, which is not on disk: %v", a.Value, err)
			}
		}
	}
	if refs == 0 {
		t.Fatalf("local manifest names no segments at all:\n%s", data)
	}
	t.Logf("local manifest resolves %d references", refs)
}
