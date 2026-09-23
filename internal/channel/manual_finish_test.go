package channel

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocfg "github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/hlskey"
)

// The source advertises three segments, but no fetch can finish until release.
// This fixes the cutover instant independently of scheduling and disk speed.
func gatedFinishChannel(t *testing.T, finished bool) (*Manager, *Runtime, *Store, <-chan struct{}, func(), *atomic.Int32) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	var requests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nmedia.m3u8\n")
	})
	mux.HandleFunc("/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "#EXTINF:1,\nseg%d.ts\n", i)
		}
		if finished {
			fmt.Fprint(w, "#EXT-X-ENDLIST\n")
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Write(tsSegmentBytes(1))
	})
	origin := httptest.NewServer(mux)
	t.Cleanup(origin.Close)
	cfg := gocfg.Config{
		Server: gocfg.ServerConfig{BaseURL: "http://gw.test"},
		Store:  gocfg.StoreConfig{DataDir: t.TempDir()},
		Worker: gocfg.WorkerConfig{FetchWorkers: 1, MaxRetriesStatic: 1},
		Window: gocfg.WindowConfig{Depth: time.Hour},
	}
	st, err := NewStore(filepath.Join(cfg.Store.DataDir, "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg)
	m.SetStore(st)
	t.Cleanup(m.StopAll)
	done := make(chan struct{})
	m.SetOnVODReady(func(rt *Runtime) { close(done) })
	cc := Config{SourceType: SourceHLS, MPDURL: origin.URL + "/master.m3u8", Enabled: true}
	if err := st.Put("finish", cc); err != nil {
		t.Fatal(err)
	}
	rt, err := m.Start(context.Background(), "finish", cc)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for rt.Broker.Len() != 2 {
		if time.Now().After(deadline) {
			t.Fatal("one in-flight and two queued segments never established")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return m, rt, st, done, release, &requests
}

func TestManualVODDrainsAdmittedSegmentsBeforePublishing(t *testing.T) {
	m, rt, st, done, release, requests := gatedFinishChannel(t, false)
	if err := m.TransitionToVOD(rt.ID); err != nil {
		t.Fatal(err)
	}
	if rt.Mode() != ModeTransitioning {
		t.Errorf("accepted transition mode = %v", rt.Mode())
	}
	if err := m.TransitionToVOD(rt.ID); err == nil {
		t.Error("duplicate transition accepted")
	}
	select {
	case <-done:
		t.Fatal("VOD published while admitted fetch was blocked")
	case <-time.After(150 * time.Millisecond):
	}
	if saved, _ := st.Get(rt.ID); saved.IsVOD {
		t.Fatal("IsVOD persisted before draining")
	}
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("admitted work did not drain")
	}
	if !rt.Broker.Idle() {
		t.Fatal("VOD callback ran with pending work")
	}
	if got := storedSegments(rt.VODIndex()); got != 3 {
		t.Fatalf("VOD has %d segments, want all 3 admitted segments", got)
	}
	if saved, _ := st.Get(rt.ID); !saved.IsVOD {
		t.Fatal("completed VOD state not persisted")
	}
	data, err := os.ReadFile(filepath.Join(m.gwCfg.Store.DataDir, rt.ID, "local_0.m3u8"))
	if err != nil || strings.Count(string(data), "#EXTINF:") != 3 {
		t.Fatalf("saved playlist missing admitted segments: %v\n%s", err, data)
	}
	// HLS target duration is one second; allow more than two poll intervals.
	before := requests.Load()
	time.Sleep(1200 * time.Millisecond)
	if requests.Load() != before {
		t.Fatal("source still polling after VOD was published")
	}
}

func TestStopInterruptsManualVODDrain(t *testing.T) {
	m, rt, st, done, _, _ := gatedFinishChannel(t, false)
	if err := m.TransitionToVOD(rt.ID); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- m.Stop(rt.ID) }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop hung behind VOD drain")
	}
	select {
	case <-done:
		t.Fatal("canceled drain published VOD")
	default:
	}
	if saved, _ := st.Get(rt.ID); saved.IsVOD {
		t.Fatal("canceled drain persisted VOD")
	}
}

func TestAutomaticVODKeepsStaticIngestingWhileDraining(t *testing.T) {
	m, rt, _, done, release, _ := gatedFinishChannel(t, true)
	waitForMode(t, rt, ModeStaticIngesting, 3*time.Second)
	// A manual request during automatic draining must still be accepted and
	// converge on one callback, without abandoning already admitted work.
	if err := m.TransitionToVOD(rt.ID); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("manual/automatic completion did not converge")
	}
	if got := storedSegments(rt.VODIndex()); got != 3 {
		t.Fatalf("VOD has %d segments, want 3", got)
	}
}

func TestRestartInterruptsManualVODDrain(t *testing.T) {
	m, rt, st, done, _, _ := gatedFinishChannel(t, false)
	if err := m.TransitionToVOD(rt.ID); err != nil {
		t.Fatal(err)
	}
	restarted := make(chan error, 1)
	go func() { restarted <- m.Restart(context.Background(), rt.ID) }()
	select {
	case err := <-restarted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Restart hung behind VOD drain")
	}
	if next := m.Get(rt.ID); next == nil || next == rt || next.Mode() != ModeLive {
		t.Fatal("Restart did not replace the interrupted runtime with live ingest")
	}
	select {
	case <-done:
		t.Fatal("interrupted runtime published VOD")
	default:
	}
	if saved, _ := st.Get(rt.ID); saved.IsVOD {
		t.Fatal("interrupted runtime persisted VOD")
	}
}

func failedVODRebuild(t *testing.T) (*Manager, *Runtime, *Store, string) {
	t.Helper()
	m := managerForTransition(t)
	t.Cleanup(m.StopAll)
	st, err := NewStore(filepath.Join(m.gwCfg.Store.DataDir, "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	m.SetStore(st)
	if err := st.Put("broken", deadChannel()); err != nil {
		t.Fatal(err)
	}
	rt := registerRuntime(t, m, "broken", ModeLive)
	blocker := filepath.Join(m.gwCfg.Store.DataDir, rt.ID, "periods")
	if err := os.MkdirAll(filepath.Dir(blocker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.TransitionToVOD(rt.ID); err != nil {
		t.Fatal(err)
	}
	// The public request has registered its finalizer before returning.
	rt.wg.Wait()
	if rt.Mode() != ModeVODFailed || rt.VODIndex() != nil {
		t.Fatal("failed rebuild advertised live ingest or a completed VOD")
	}
	select {
	case <-rt.vodDone:
		t.Fatal("failed rebuild signaled completion")
	default:
	}
	if saved, _ := st.Get(rt.ID); saved.IsVOD {
		t.Fatal("failed rebuild persisted VOD")
	}
	return m, rt, st, blocker
}

func TestVODRebuildFailureCanBeRecoveredByRestart(t *testing.T) {
	m, rt, _, blocker := failedVODRebuild(t)
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := m.Restart(context.Background(), rt.ID); err != nil {
		t.Fatal(err)
	}
	if next := m.Get(rt.ID); next == nil || next == rt || next.Mode() != ModeLive {
		t.Fatal("Restart did not recover the failed transition")
	}
}

func TestVODRebuildFailureCanBeRetried(t *testing.T) {
	m, rt, st, blocker := failedVODRebuild(t)
	if status, ok := m.StatusFor(rt.ID); !ok || status.Mode != "vod_failed" {
		t.Fatalf("status = %+v, want visible retryable failure", status)
	}
	// Retrying before the disk problem is resolved must remain retryable.
	if err := m.TransitionToVOD(rt.ID); err != nil {
		t.Fatal(err)
	}
	rt.wg.Wait()
	if rt.Mode() != ModeVODFailed {
		t.Fatalf("retry of broken disk mode = %v", rt.Mode())
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	m.SetOnVODReady(func(*Runtime) { close(done) })
	if err := m.TransitionToVOD(rt.ID); err != nil {
		t.Fatalf("retry rejected: %v", err)
	}
	rt.wg.Wait()
	if m.Get(rt.ID) != rt || rt.Mode() != ModeVOD || rt.VODIndex() == nil {
		t.Fatal("retry did not publish VOD on the existing runtime")
	}
	select {
	case <-done:
	default:
		t.Fatal("retry did not invoke VOD callback")
	}
	if saved, _ := st.Get(rt.ID); !saved.IsVOD {
		t.Fatal("successful retry did not persist VOD")
	}
	if err := m.TransitionToVOD(rt.ID); err == nil {
		t.Fatal("completed VOD accepted another retry")
	}
}

// Quiescing the processor must join key warming without canceling it: these
// disk-backed keys are required to play a passthrough VOD with origin offline.
func TestVODCompletionWaitsForPassthroughKey(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprintf("automatic=%v", automatic), func(t *testing.T) {
			keyHit, release := make(chan struct{}), make(chan struct{})
			var hitOnce, releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			mux := http.NewServeMux()
			mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nmedia.m3u8\n")
			})
			mux.HandleFunc("/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-KEY:METHOD=AES-128,URI=\"key\"\n#EXTINF:1,\nseg.ts\n")
				if automatic {
					fmt.Fprint(w, "#EXT-X-ENDLIST\n")
				}
			})
			mux.HandleFunc("/seg.ts", func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, 576)) })
			mux.HandleFunc("/key", func(w http.ResponseWriter, r *http.Request) {
				hitOnce.Do(func() { close(keyHit) })
				select {
				case <-release:
					w.Write([]byte("0123456789abcdef"))
				case <-r.Context().Done():
				}
			})
			origin := httptest.NewServer(mux)
			t.Cleanup(origin.Close)
			cfg := gocfg.Config{Store: gocfg.StoreConfig{DataDir: t.TempDir()}, Worker: gocfg.WorkerConfig{FetchWorkers: 1}, Window: gocfg.WindowConfig{Depth: time.Hour}}
			m := NewManager(cfg)
			t.Cleanup(m.StopAll)
			done := make(chan struct{})
			m.SetOnVODReady(func(*Runtime) { close(done) })
			rt, err := m.Start(context.Background(), "keys", Config{SourceType: SourceHLS, HLSKeyMode: KeyModePassthrough, MPDURL: origin.URL + "/master.m3u8", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-keyHit:
			case <-time.After(5 * time.Second):
				t.Fatal("key warming never started")
			}
			if !automatic {
				if err := m.TransitionToVOD(rt.ID); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
				t.Fatal("VOD completed before key warming finished")
			case <-time.After(200 * time.Millisecond):
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("VOD did not complete after key capture")
			}
			if data, err := os.ReadFile(hlskey.KeyStorePath(cfg.Store.DataDir, rt.ID)); err != nil || len(data) == 0 {
				t.Fatalf("key cache not persisted before completion: %v", err)
			}
		})
	}
}
