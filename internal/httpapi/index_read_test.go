package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/index"
)

func TestMediaHandlersHonorIndexPublicationRules(t *testing.T) {
	cfg := config.Config{Store: config.StoreConfig{DataDir: t.TempDir()}}
	mgr := newTestManager(t, cfg)
	t.Cleanup(mgr.StopAll)
	rt, err := createChannel(mgr, context.Background(), "c", ch.Config{MPDURL: "http://origin.invalid/a.mpd", IsVOD: true})
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ci := rt.ActiveIndex()
	ci.Period("0").AS("1").SetMediaType(index.MediaAudio) // keeps video committed
	ci.Commit(index.RepRef{PeriodID: "0", ASID: "0", RepID: "v", MediaType: index.MediaVideo}, 0, index.SegmentState{SegNo: 1, Path: write("1.ts", "pending-media"), InitPath: write("init.mp4", "pending-init")})
	h := periodRouter(t, cfg, mgr)
	if code, body := get(t, h, "/v1/channels/c/init/0/v.mp4"); code != http.StatusOK || string(body) != "pending-init" {
		t.Fatalf("committed init: %d %q", code, body)
	}
	if code, _ := get(t, h, "/v1/channels/c/segments/0/v/1.ts"); code != http.StatusNotFound {
		t.Fatalf("committed media status=%d", code)
	}
	rep := ci.FindRep("0", "0", "v")
	rep.RestorePublished(index.SegmentState{SegNo: 2, Path: write("2.ts", "published-media"), InitPath: write("init.mp4", "published-init"), EndPTS: 10, Timescale: 1})
	if code, body := get(t, h, "/v1/channels/c/init/0/v.mp4"); code != http.StatusOK || string(body) != "published-init" {
		t.Fatalf("published init: %d %q", code, body)
	}
	// The requested extension does not select the stored format.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/channels/c/segments/0/v/2.m4s", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "published-media" || rec.Header().Get("Content-Type") != "video/mp2t" {
		t.Fatalf("published media: %d %q %s", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
	}
	rep.ExpireOlderThan(11)
	if code, _ := get(t, h, "/v1/channels/c/segments/0/v/2.ts"); code != http.StatusNotFound {
		t.Fatalf("expired media status=%d", code)
	}
}
