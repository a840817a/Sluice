package channel

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a840817a/sluice/internal/hlsdesc"
	"github.com/a840817a/sluice/internal/hlskey"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/presentation"
)

func TestLocalArtifactsWriteKeysPrivatelyBeforeMaster(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "media write fails"}[blocked], func(t *testing.T) {
			dir := t.TempDir()
			key := []byte("0123456789abcdef")
			uri := "https://offline.invalid/key"
			raw, _ := json.Marshal(map[string]string{uri: hex.EncodeToString(key)})
			keyStore := filepath.Join(dir, "keys.json")
			if err := os.WriteFile(keyStore, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			cache := hlskey.NewCache(nil, nil, keyStore)
			ci := index.NewChannelIndex(index.PublishAVBarrier)
			segPath := filepath.Join(dir, "segment.ts")
			if err := os.WriteFile(segPath, []byte("media"), 0o644); err != nil {
				t.Fatal(err)
			}
			ci.Period("0").AS("0").Rep("v0", 0).RestorePublished(index.SegmentState{SegNo: 1, StartPTS: 0, EndPTS: 2, Timescale: 1, Path: segPath, KeyURI: uri, IV: make([]byte, 16)})
			if blocked {
				if err := os.Mkdir(filepath.Join(dir, "local_0.m3u8"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			a := vodArtifacts{dir: dir, render: &presentation.Generator{}, keys: cache, snapshot: ci.Snapshot(), serving: Serving{Channel: presentation.Channel{Index: ci, IsHLSSource: true, Tracks: presentation.Tracks{Variants: []hlsdesc.Variant{{RepID: "v0", Bandwidth: 10}}}}}}
			a.writeOptional()
			keyFile := filepath.Join(dir, "local_key_"+hlskey.KeyID(uri)+".key")
			stat, err := os.Stat(keyFile)
			if err != nil {
				t.Fatal(err)
			}
			if stat.Mode().Perm() != 0o600 {
				t.Fatalf("key permission %o", stat.Mode().Perm())
			}
			got, err := os.ReadFile(keyFile)
			if err != nil || string(got) != string(key) {
				t.Fatal("wrong stored key")
			}
			master, err := os.ReadFile(filepath.Join(dir, "local.m3u8"))
			if blocked {
				if !os.IsNotExist(err) {
					t.Fatalf("published master after child write failed: %v", err)
				}
				return
			}
			if err != nil || !strings.Contains(string(master), "local_0.m3u8") {
				t.Fatalf("master: %s %v", master, err)
			}
			media, err := os.ReadFile(filepath.Join(dir, "local_0.m3u8"))
			if err != nil || !strings.Contains(string(media), filepath.Base(keyFile)) {
				t.Fatalf("media: %s %v", media, err)
			}
		})
	}
}
