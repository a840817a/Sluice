package index

import (
	"encoding/binary"
	"os"
	"testing"

	"github.com/a840817a/sluice/internal/store"
)

func minimalInit(timescale uint32) []byte {
	box := func(name string, body []byte) []byte {
		b := binary.BigEndian.AppendUint32(nil, uint32(8+len(body)))
		b = append(b, name...)
		return append(b, body...)
	}
	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:16], timescale)
	return box("moov", box("trak", box("mdia", box("mdhd", mdhd))))
}

// Invalid init must not become playable merely because the gateway restarted
// or entered VOD. An excluded audio track must not block usable video either.
func TestRebuildExcludesUnreadyInit(t *testing.T) {
	for _, mode := range []string{"live", "vod"} {
		for _, bad := range []string{"missing", "corrupt", "no mdhd", "zero timescale"} {
			t.Run(mode+"/"+bad, func(t *testing.T) {
				dir := t.TempDir()
				good := store.RepLocation{DataDir: dir, ChannelID: "ch", PeriodID: "p", MediaType: MediaVideo, ASID: "0", RepID: "v"}
				invalid := store.RepLocation{DataDir: dir, ChannelID: "ch", PeriodID: "p", MediaType: MediaAudio, ASID: "1", RepID: "a"}
				for _, loc := range []store.RepLocation{good, invalid} {
					if err := store.Write(loc.SegmentPath(1, store.ExtFMP4), minimalFMP4(0)); err != nil {
						t.Fatal(err)
					}
				}
				if err := store.Write(good.InitPath(), minimalInit(90000)); err != nil {
					t.Fatal(err)
				}
				var data []byte
				switch bad {
				case "corrupt":
					data = []byte("broken")
				case "no mdhd":
					data = []byte{0, 0, 0, 8, 'm', 'o', 'o', 'v'}
				case "zero timescale":
					data = minimalInit(0)
				}
				if data != nil {
					if err := os.WriteFile(invalid.InitPath(), data, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				ci := NewChannelIndex(PublishAVBarrier)
				var err error
				if mode == "vod" {
					err = RebuildFromDiskForVOD(ci, dir, "ch")
				} else {
					err = RebuildFromDisk(ci, dir, "ch")
				}
				if err != nil {
					t.Fatal(err)
				}
				if rep := ci.FindRep("p", "1", "a"); rep != nil && len(rep.Published())+len(rep.Committed()) != 0 {
					t.Fatal("unready audio was rebuilt")
				}
				rep := ci.FindRep("p", "0", "v")
				if rep == nil || len(rep.Published()) != 1 || rep.Published()[0].Timescale != 90000 {
					t.Fatal("usable video was not published")
				}
			})
		}
	}
}
