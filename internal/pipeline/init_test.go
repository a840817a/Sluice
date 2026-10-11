package pipeline

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/progress"
	"github.com/a840817a/sluice/internal/queue"
	"github.com/a840817a/sluice/internal/store"
)

type initPipeline struct {
	ci     *index.ChannelIndex
	broker *queue.Broker
	prog   *progress.Counters
	stop   func()
}

func startInitPipeline(t *testing.T, dir string, tasks ...*queue.SegmentTask) initPipeline {
	t.Helper()
	cfg := config.Config{Store: config.StoreConfig{DataDir: dir, KeepAllSegments: true}, Worker: config.WorkerConfig{FetchWorkers: 2, MaxRetriesStatic: 1}}
	ci := index.NewChannelIndex(index.PublishImmediately)
	if err := index.RebuildFromDiskForVOD(ci, dir, "ch"); err != nil {
		t.Fatal(err)
	}
	broker := queue.NewBroker(100)
	for _, task := range tasks {
		if !broker.PushPreserving(t.Context(), task) {
			t.Fatal("admission failed")
		}
	}
	pool := fetch.NewPool(fetch.NewFetcher(nil, nil, 1<<20, time.Second), broker, cfg.Worker.FetchWorkers)
	p := NewProcessor(cfg, "ch", ci, broker)
	prog := progress.New()
	p.SetProgress(prog)
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pool.Run(ctx) }()
	go func() { defer wg.Done(); p.Run(ctx, pool) }()
	stop := func() {
		cancel()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("init work did not join on Stop")
		}
	}
	t.Cleanup(stop)
	return initPipeline{ci: ci, broker: broker, prog: prog, stop: stop}
}

func initTask(n uint64, origin string) *queue.SegmentTask {
	task := videoTask(n, queue.FormatFMP4)
	task.URL = fmt.Sprintf("%s/media/%d", origin, n)
	task.InitURL = origin + "/init"
	task.Duration, task.Timescale = 2, 1
	return task
}

func awaitInitCondition(t *testing.T, message string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal(message)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestInitWaitBoundsMediaAndPublishesWithTrackTimescale(t *testing.T) {
	var media, inits atomic.Int32
	ready := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(ready) }) })
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/init" {
			inits.Add(1)
			select {
			case <-ready:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write(fmp4Init(90000))
		} else {
			media.Add(1)
			_, _ = w.Write(fmp4Segment(180000))
		}
	}))
	t.Cleanup(origin.Close)
	dir := t.TempDir()
	var tasks []*queue.SegmentTask
	for n := uint64(1); n <= 30; n++ {
		tasks = append(tasks, initTask(n, origin.URL))
	}
	pl := startInitPipeline(t, dir, tasks...)
	// Two pending + eight pool results + two workers holding a body. The
	// independent init request still completes when all twelve slots fill.
	awaitInitCondition(t, "media did not fill the bounded wait", func() bool { return media.Load() >= 12 })
	if pl.broker.Idle() {
		t.Fatal("waiting media was released before init completed")
	}
	if _, err := os.Stat(location(dir, tasks[0]).SegmentPath(1, store.ExtFMP4)); !os.IsNotExist(err) {
		t.Fatal("media reached disk before init")
	}
	if rep := pl.ci.FindRep("0", "0", "v0"); rep != nil && len(rep.Published()) != 0 {
		t.Fatal("published before init")
	}
	// No consumer can make room until ready is closed.
	time.Sleep(30 * time.Millisecond)
	if media.Load() != 12 {
		t.Fatalf("unbounded media downloads: %d", media.Load())
	}
	once.Do(func() { close(ready) })
	awaitInitCondition(t, "ingest did not drain after init", pl.broker.Idle)
	pl.stop()
	if inits.Load() != 1 {
		t.Fatalf("init fetched %d times", inits.Load())
	}
	rep := pl.ci.FindRep("0", "0", "v0")
	if rep == nil || len(rep.Published()) != 30 {
		t.Fatal("missing published media")
	}
	for _, s := range rep.Published() {
		if s.Timescale != 90000 || s.StartPTS != 180000 || s.EndPTS != 360000 {
			t.Fatalf("wrong timing: %+v", s)
		}
	}
	for _, task := range tasks {
		if task.Retries != 0 {
			t.Fatal("waiting consumed media retry budget")
		}
	}
}

func TestInitRetriesWithoutRefetchingMedia(t *testing.T) {
	for _, failure := range []string{"http", "invalid", "zero timescale", "write"} {
		t.Run(failure, func(t *testing.T) {
			var media, inits atomic.Int32
			dir := t.TempDir()
			loc := location(dir, videoTask(1, queue.FormatFMP4))
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/init" {
					media.Add(1)
					_, _ = w.Write(fmp4Segment(180000))
					return
				}
				n := inits.Add(1)
				if n == 1 {
					switch failure {
					case "http":
						w.WriteHeader(503)
						return
					case "invalid":
						_, _ = w.Write(mp4box("moov"))
						return
					case "zero timescale":
						_, _ = w.Write(fmp4Init(0))
						return
					case "write":
						if err := os.MkdirAll(loc.InitPath(), 0o755); err != nil {
							t.Error(err)
						}
					}
				} else if failure == "write" {
					if err := os.Remove(loc.InitPath()); err != nil {
						t.Error(err)
					}
				}
				_, _ = w.Write(fmp4Init(90000))
			}))
			t.Cleanup(origin.Close)
			task := initTask(1, origin.URL)
			pl := startInitPipeline(t, dir, task)
			awaitInitCondition(t, "init retry did not drain", pl.broker.Idle)
			pl.stop()
			if media.Load() != 1 || inits.Load() != 2 || task.Retries != 0 {
				t.Fatalf("media=%d init=%d retries=%d", media.Load(), inits.Load(), task.Retries)
			}
			if s := onlySegment(t, pl.ci, task); s.Timescale != 90000 {
				t.Fatal("bad timescale")
			}
		})
	}
}

func TestInitReuseAndMissingSource(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprint(local), func(t *testing.T) {
			var inits atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/init" {
					inits.Add(1)
					w.WriteHeader(410)
					return
				}
				_, _ = w.Write(fmp4Segment(180000))
			}))
			t.Cleanup(origin.Close)
			dir := t.TempDir()
			task := initTask(1, origin.URL)
			if local {
				if err := store.Write(location(dir, task).InitPath(), fmp4Init(90000)); err != nil {
					t.Fatal(err)
				}
			} else {
				task.InitURL = ""
			}
			pl := startInitPipeline(t, dir, task)
			awaitInitCondition(t, "init reuse/missing source did not drain", pl.broker.Idle)
			pl.stop()
			if inits.Load() != 0 {
				t.Fatal("contacted origin despite local init / missing URL")
			}
			if local {
				_ = onlySegment(t, pl.ci, task)
			} else {
				if _, err := os.Stat(location(dir, task).SegmentPath(1, store.ExtFMP4)); !os.IsNotExist(err) {
					t.Fatal("stored media with no init source")
				}
				if pl.prog.Snapshot().LastSegmentError == nil {
					t.Fatal("missing init failure was not reported")
				}
			}
		})
	}
}

func TestInitFailureAndDeadlineDoNotBlockOtherRepresentations(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/init" {
					if deadline {
						<-r.Context().Done()
					} else {
						_, _ = w.Write(mp4box("moov"))
					}
					return
				}
				_, _ = w.Write(fmp4Segment(180000))
			}))
			t.Cleanup(origin.Close)
			dir := t.TempDir()
			bad := initTask(1, origin.URL)
			if deadline {
				bad.Deadline = time.Now().Add(200 * time.Millisecond)
			}
			good := initTask(1, origin.URL)
			good.RepID = "good"
			if err := store.Write(location(dir, good).InitPath(), fmp4Init(90000)); err != nil {
				t.Fatal(err)
			}
			began := time.Now()
			pl := startInitPipeline(t, dir, bad, good)
			awaitInitCondition(t, "failed init blocked drain", pl.broker.Idle)
			pl.stop()
			if deadline && time.Since(began) > time.Second {
				t.Fatal("live deadline waited for the HTTP timeout/retry budget")
			}
			if _, err := os.Stat(location(dir, bad).SegmentPath(1, store.ExtFMP4)); !os.IsNotExist(err) {
				t.Fatal("unready media stored")
			}
			_ = onlySegment(t, pl.ci, good)
		})
	}
}

func TestStopCancelsInitAndJoins(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	var startedOnce, cancelledOnce sync.Once
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/init" {
			startedOnce.Do(func() { close(started) })
			<-r.Context().Done()
			cancelledOnce.Do(func() { close(cancelled) })
			return
		}
		_, _ = w.Write(fmp4Segment(180000))
	}))
	t.Cleanup(origin.Close)
	dir := t.TempDir()
	task := initTask(1, origin.URL)
	pl := startInitPipeline(t, dir, task)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("init did not start")
	}
	pl.stop()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("init request not cancelled")
	}
	if _, err := os.Stat(location(dir, task).SegmentPath(1, store.ExtFMP4)); !os.IsNotExist(err) {
		t.Fatal("stopped ingest wrote waiting media")
	}
	if !pl.broker.Idle() {
		t.Fatal("cancelled init wait leaked its media attempt")
	}
}

func TestInitReadinessIsScopedToPeriod(t *testing.T) {
	var inits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/init" {
			inits.Add(1)
			_, _ = w.Write(fmp4Init(48000))
		} else {
			_, _ = w.Write(fmp4Segment(96000))
		}
	}))
	t.Cleanup(origin.Close)
	dir := t.TempDir()
	old := initTask(1, origin.URL)
	if err := store.Write(location(dir, old).InitPath(), fmp4Init(90000)); err != nil {
		t.Fatal(err)
	}
	fresh := initTask(1, origin.URL)
	fresh.PeriodID = "1"
	pl := startInitPipeline(t, dir, old, fresh)
	awaitInitCondition(t, "periods did not drain", pl.broker.Idle)
	pl.stop()
	if inits.Load() != 1 {
		t.Fatalf("init fetches = %d, want one new period", inits.Load())
	}
	if onlySegment(t, pl.ci, old).Timescale != 90000 || onlySegment(t, pl.ci, fresh).Timescale != 48000 {
		t.Fatal("init readiness leaked between periods")
	}
}

func TestIntakeRefusesUnreadyInitBeforeWritingMedia(t *testing.T) {
	for _, data := range [][]byte{nil, mp4box("moov"), fmp4Init(0)} {
		in, ci, dir := newTestIntake(t)
		task := videoTask(1, queue.FormatFMP4)
		if data != nil {
			if err := store.Write(location(dir, task).InitPath(), data); err != nil {
				t.Fatal(err)
			}
		}
		out := in.Store(t.Context(), task, fmp4Segment(0))
		if out.Stored || out.Err == nil || out.Stage != "init" {
			t.Fatalf("unready init was accepted: %+v", out)
		}
		if _, err := os.Stat(location(dir, task).SegmentPath(1, store.ExtFMP4)); !os.IsNotExist(err) {
			t.Fatal("unready media reached final path")
		}
		if rep := ci.FindRep("0", "0", "v0"); rep != nil && len(rep.Published()) != 0 {
			t.Fatal("unready media published")
		}
	}
}

func TestInitReusesLegacyDirectoryAfterRestart(t *testing.T) {
	var inits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/init" {
			inits.Add(1)
			w.WriteHeader(410)
			return
		}
		_, _ = w.Write(fmp4Segment(180000))
	}))
	t.Cleanup(origin.Close)
	dir := t.TempDir()
	legacy := filepath.Join(dir, "ch", "periods", "p:1", "video_0", "v:0")
	if err := store.Write(filepath.Join(legacy, "init.mp4"), fmp4Init(90000)); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(filepath.Join(legacy, "1.m4s"), fmp4Segment(0)); err != nil {
		t.Fatal(err)
	}
	task := initTask(2, origin.URL)
	task.PeriodID, task.RepID = "p:1", "v:0"
	pl := startInitPipeline(t, dir, task)
	awaitInitCondition(t, "legacy init reuse did not drain", pl.broker.Idle)
	pl.stop()
	if inits.Load() != 0 {
		t.Fatal("legacy init was not reused")
	}
	rep := pl.ci.FindRep("p:1", "0", "v:0")
	if rep == nil || len(rep.Published()) != 2 {
		t.Fatal("restored/new segments missing")
	}
	for _, seg := range rep.Published() {
		if _, err := os.Stat(seg.InitPath); err != nil {
			t.Fatalf("unresolvable init after restore: %v", err)
		}
	}
}
