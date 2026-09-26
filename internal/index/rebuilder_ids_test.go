package index

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/a840817a/sluice/internal/store"
)

// The rebuild reads period and representation IDs back from the directories
// the store named after them (ADR-0006): encoded names decode to the origin
// ID, and a raw name written before the encoding existed still reads as the ID
// it was.
func TestRebuildReadsIDsBackFromDirectoryNames(t *testing.T) {
	dataDir := t.TempDir()
	put := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, store.InitFileName), minimalFMP4(0), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "1.m4s"), minimalFMP4(0), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Written through the store: IDs a raw path join would drop or resolve.
	put(filepath.Dir(store.RepLocation{DataDir: dataDir, ChannelID: "ch", PeriodID: "", MediaType: "video", ASID: "0", RepID: "v0"}.InitPath()))
	put(filepath.Dir(store.RepLocation{DataDir: dataDir, ChannelID: "ch", PeriodID: "..", MediaType: "video", ASID: "0", RepID: "a/b"}.InitPath()))
	// Written before the encoding: the raw ID as the directory name.
	put(filepath.Join(dataDir, "ch", "periods", "p 1", "video_0", "r:1"))
	// A raw ID that looks like an encoding of another ID ("~YQ" is "a"
	// encoded) is still that raw ID: the store never writes "a" as "~YQ".
	put(filepath.Join(dataDir, "ch", "periods", "~YQ", "video_0", "v0"))

	ci := NewChannelIndex(PublishAVBarrier)
	if err := RebuildFromDisk(ci, dataDir, "ch"); err != nil {
		t.Fatalf("RebuildFromDisk: %v", err)
	}
	for _, want := range []struct{ period, rep string }{{"", "v0"}, {"..", "a/b"}, {"p 1", "r:1"}, {"~YQ", "v0"}} {
		if ci.FindRep(want.period, "0", want.rep) == nil {
			t.Errorf("representation %q of period %q not rebuilt; periods: %q", want.rep, want.period, ci.PeriodIDs())
		}
	}
}
