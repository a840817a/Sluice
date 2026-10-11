package pipeline

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/a840817a/sluice/internal/config"
	"github.com/a840817a/sluice/internal/fetch"
	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/progress"
	"github.com/a840817a/sluice/internal/queue"
	"github.com/a840817a/sluice/internal/store"
)

func mp4box(typ string, payload ...[]byte) []byte {
	var body []byte
	for _, p := range payload {
		body = append(body, p...)
	}
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	out = append(out, []byte(typ)...)
	return append(out, body...)
}

// fmp4Init is a minimal init segment whose mdhd box carries timescale.
func fmp4Init(timescale uint32) []byte {
	ts := make([]byte, 4)
	binary.BigEndian.PutUint32(ts, timescale)
	mdhd := []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	mdhd = append(mdhd, ts...)
	mdhd = append(mdhd, 0, 0, 0, 0, 0x55, 0xc4, 0, 0)
	return mp4box("moov", mp4box("trak", mp4box("mdia", mp4box("mdhd", mdhd))))
}

// fmp4Segment is a minimal media segment whose tfdt box carries bmdt.
func fmp4Segment(bmdt uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, bmdt)
	moof := mp4box("moof", mp4box("traf", mp4box("tfdt", append([]byte{1, 0, 0, 0}, b...))))
	return append(moof, mp4box("mdat", []byte{0, 0, 0, 0})...)
}

// tsSegment is three MPEG-TS packets.
func tsSegment() []byte {
	out := make([]byte, 3*188)
	for p := 0; p < 3; p++ {
		out[p*188] = 0x47
		for i := 1; i < 188; i++ {
			out[p*188+i] = byte(p + i)
		}
	}
	return out
}

func encrypt(t *testing.T, plain, key, iv []byte) []byte {
	t.Helper()
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out
}

type fakeKeys struct {
	key []byte
	err error
}

func (f fakeKeys) Get(context.Context, string) ([]byte, error) { return f.key, f.err }

func newTestIntake(t *testing.T) (*Intake, *index.ChannelIndex, string) {
	t.Helper()
	dir := t.TempDir()
	ci := index.NewChannelIndex(index.PublishImmediately)
	return NewIntake(config.Config{Store: config.StoreConfig{DataDir: dir}}, "ch", ci), ci, dir
}

func videoTask(segNo uint64, format queue.SegmentFormat) *queue.SegmentTask {
	return &queue.SegmentTask{
		ChannelID: "ch", PeriodID: "0", ASID: "0", RepID: "v0", MediaType: index.MediaVideo,
		SegNo: segNo, Format: format,
	}
}

func location(dir string, task *queue.SegmentTask) store.RepLocation {
	return store.RepLocation{DataDir: dir, ChannelID: "ch", PeriodID: task.PeriodID, MediaType: task.MediaType, ASID: task.ASID, RepID: task.RepID}
}

// onlySegment returns the single segment ci holds for task's representation.
func onlySegment(t *testing.T, ci *index.ChannelIndex, task *queue.SegmentTask) index.SegmentState {
	t.Helper()
	rep := ci.FindRep(task.PeriodID, task.ASID, task.RepID)
	if rep == nil {
		t.Fatal("representation not in the index")
	}
	segs := append(rep.Committed(), rep.Published()...)
	if len(segs) != 1 {
		t.Fatalf("index holds %d segments, want 1: %+v", len(segs), segs)
	}
	return segs[0]
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An fMP4 segment is timed in its track's timescale, taken from the init
// segment's mdhd box, not in the SegmentTemplate's.
func TestStoreFMP4(t *testing.T) {
	in, ci, dir := newTestIntake(t)
	task := videoTask(7, queue.FormatFMP4)
	task.Duration, task.Timescale = 2, 1 // two seconds, in the template's timescale
	data := fmp4Segment(180000)

	if err := store.Write(location(dir, task).InitPath(), fmp4Init(90000)); err != nil {
		t.Fatal(err)
	}
	out := in.Store(context.Background(), task, data)
	if !out.Stored || !out.New || out.Published != 1 || out.Bytes != len(data) {
		t.Fatalf("Outcome = %+v, want stored, new, 1 published, %d bytes", out, len(data))
	}
	loc := location(dir, task)
	if !bytes.Equal(readFile(t, loc.SegmentPath(7, store.ExtFMP4)), data) {
		t.Error("segment file does not hold the fetched bytes")
	}
	readFile(t, loc.InitPath())

	seg := onlySegment(t, ci, task)
	if seg.Timescale != 90000 || seg.StartPTS != 180000 || seg.EndPTS != 180000+2*90000 {
		t.Errorf("timing = %d..%d @%d, want 180000..360000 @90000", seg.StartPTS, seg.EndPTS, seg.Timescale)
	}
	if seg.InitPath != loc.InitPath() {
		t.Errorf("InitPath = %q, want %q", seg.InitPath, loc.InitPath())
	}

	// Fetched again, as the HLS backfill path can: stored, but not new.
	if again := in.Store(context.Background(), task, data); !again.Stored || again.New {
		t.Errorf("second Store = %+v, want stored and not new", again)
	}
}

// An MPEG-TS segment has no timing of its own: it comes from the task and is
// recorded in the sidecar so a restart can rebuild it.
func TestStoreTS(t *testing.T) {
	in, ci, dir := newTestIntake(t)
	task := videoTask(3, queue.FormatTS)
	task.StartPTS, task.Duration, task.Timescale = 900, 4, 1
	task.PersistMeta, task.Discontinuity = true, true

	if out := in.Store(context.Background(), task, tsSegment()); !out.Stored {
		t.Fatalf("Outcome = %+v, want stored", out)
	}
	loc := location(dir, task)
	readFile(t, loc.SegmentPath(3, store.ExtTS))
	seg := onlySegment(t, ci, task)
	if seg.StartPTS != 900 || seg.InitPath != "" || !seg.Discontinuity {
		t.Errorf("segment = %+v, want StartPTS 900, no init, discontinuity", seg)
	}
	records, err := store.ReadSegmentMeta(loc.SidecarPath())
	if err != nil || len(records) != 1 || records[0].SegNo != 3 || records[0].StartPTS != 900 || !records[0].Discontinuity {
		t.Errorf("sidecar = %+v, %v; want one record for segment 3 at 900 with its discontinuity", records, err)
	}
}

// Passthrough keeps ciphertext, which can only be checked for length, and
// keeps the key material the playlist must re-advertise.
func TestStorePassthroughCiphertext(t *testing.T) {
	in, ci, dir := newTestIntake(t)
	task := videoTask(1, queue.FormatTS)
	iv := bytes.Repeat([]byte{9}, 16)
	task.Encrypted, task.KeyURI, task.IV, task.PersistMeta = true, "https://keys/k1", iv, true
	cipherText := bytes.Repeat([]byte{0xAB}, 32) // no sync byte anywhere

	if out := in.Store(context.Background(), task, cipherText); !out.Stored {
		t.Fatalf("Outcome = %+v, want stored", out)
	}
	if !bytes.Equal(readFile(t, location(dir, task).SegmentPath(1, store.ExtTS)), cipherText) {
		t.Error("ciphertext was not stored as fetched")
	}
	if seg := onlySegment(t, ci, task); seg.KeyURI != task.KeyURI || !bytes.Equal(seg.IV, iv) {
		t.Errorf("segment key = %q %x, want %q %x", seg.KeyURI, seg.IV, task.KeyURI, iv)
	}
	records, _ := store.ReadSegmentMeta(location(dir, task).SidecarPath())
	if len(records) != 1 || records[0].IV != hex.EncodeToString(iv) {
		t.Errorf("sidecar = %+v, want the IV recorded", records)
	}

	if out := in.Store(context.Background(), videoTaskEncrypted(2), bytes.Repeat([]byte{1}, 33)); out.Stored || out.Stage != StageValidate {
		t.Errorf("ciphertext of a non-block length: Outcome = %+v, want refused at validate", out)
	}
}

func videoTaskEncrypted(segNo uint64) *queue.SegmentTask {
	task := videoTask(segNo, queue.FormatTS)
	task.Encrypted, task.KeyURI, task.IV = true, "https://keys/k1", make([]byte, 16)
	return task
}

// A segment decrypted on ingest is validated as the media it is, stored as
// cleartext, and carries no key material.
func TestStoreDecryptOnIngest(t *testing.T) {
	in, ci, dir := newTestIntake(t)
	key, iv := bytes.Repeat([]byte{7}, 16), bytes.Repeat([]byte{3}, 16)
	in.SetKeyFetcher(fakeKeys{key: key})
	task := videoTask(1, queue.FormatTS)
	task.Encrypted, task.DecryptOnIngest, task.KeyURI, task.IV = true, true, "https://keys/k1", iv
	plain := tsSegment()

	out := in.Store(context.Background(), task, encrypt(t, plain, key, iv))
	if !out.Stored || out.Bytes != len(plain) {
		t.Fatalf("Outcome = %+v, want stored with %d bytes", out, len(plain))
	}
	if !bytes.Equal(readFile(t, location(dir, task).SegmentPath(1, store.ExtTS)), plain) {
		t.Error("stored bytes are not the cleartext")
	}
	if seg := onlySegment(t, ci, task); seg.KeyURI != "" || seg.IV != nil {
		t.Errorf("cleartext segment carries key %q %x", seg.KeyURI, seg.IV)
	}

	in.SetKeyFetcher(fakeKeys{err: errors.New("key server down")})
	task2 := *task
	task2.SegNo = 2
	if out := in.Store(context.Background(), &task2, encrypt(t, plain, key, iv)); out.Stored || out.Stage != StageDecrypt || out.Retry {
		t.Errorf("key server down: Outcome = %+v, want refused at decrypt without retry", out)
	}
}

// A segment that is not the media it claims to be is dropped before anything
// reaches disk or the index.
func TestStoreRefusesInvalidMedia(t *testing.T) {
	in, ci, dir := newTestIntake(t)
	for _, task := range []*queue.SegmentTask{videoTask(1, queue.FormatFMP4), videoTask(2, queue.FormatTS), videoTask(3, queue.FormatWebVTT)} {
		out := in.Store(context.Background(), task, []byte("<html>origin error</html>"))
		if out.Stored || out.Stage != StageValidate || out.Err == nil || out.Retry {
			t.Errorf("format %v: Outcome = %+v, want refused at validate without retry", task.Format, out)
		}
	}
	if rep := ci.FindRep("0", "0", "v0"); rep != nil {
		t.Errorf("index gained a representation for refused segments")
	}
	if _, err := os.Stat(filepath.Join(dir, "ch")); !os.IsNotExist(err) {
		t.Errorf("refused segments left files under the channel (stat err %v)", err)
	}
}

// A write that fails is the gateway's problem, not the origin's: the segment
// is worth fetching again.
func TestStoreRetriesAFailedWrite(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ci := index.NewChannelIndex(index.PublishImmediately)
	in := NewIntake(config.Config{Store: config.StoreConfig{DataDir: notADir}}, "ch", ci)

	out := in.Store(context.Background(), videoTask(1, queue.FormatTS), tsSegment())
	if out.Stored || out.Stage != StageWrite || !out.Retry {
		t.Errorf("Outcome = %+v, want refused at write with retry", out)
	}
	if ci.FindRep("0", "0", "v0") != nil {
		t.Error("a segment that was never written reached the index")
	}
}

// Storing a segment expires what its arrival pushed out of the window.
func TestStoreExpiresWhatLeftTheWindow(t *testing.T) {
	dir := t.TempDir()
	ci := index.NewChannelIndex(index.PublishImmediately)
	in := NewIntake(config.Config{
		Store:  config.StoreConfig{DataDir: dir},
		Window: config.WindowConfig{Depth: 4 * time.Second},
	}, "ch", ci)
	for n := uint64(0); n < 6; n++ {
		task := videoTask(n, queue.FormatTS)
		task.StartPTS, task.Duration, task.Timescale = int64(n)*2, 2, 1
		if out := in.Store(context.Background(), task, tsSegment()); !out.Stored {
			t.Fatalf("segment %d: %+v", n, out)
		}
	}
	// Live edge 12s, window 4s: segments ending at or before 8s have expired.
	published, _, expired := ci.FindRep("0", "0", "v0").StatusCounts()
	if expired == 0 || published+expired != 6 {
		t.Errorf("published %d expired %d, want the oldest expired out of 6", published, expired)
	}
}

// The Processor acts on what its Intake reports: a failed write goes back to
// the queue, a refused segment does not, and only a new segment is counted.
func TestProcessorActsOnTheOutcome(t *testing.T) {
	for _, tc := range []struct {
		name        string
		dataDirFile bool
		data        []byte
		wantRequeue bool
		wantStored  uint64
	}{
		{name: "stored", data: tsSegment(), wantStored: 1},
		{name: "refused", data: []byte("not media")},
		{name: "write failed", dataDirFile: true, data: tsSegment(), wantRequeue: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.dataDirFile {
				dir = filepath.Join(dir, "file")
				if err := os.WriteFile(dir, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			broker := queue.NewBroker(4)
			prog := progress.New()
			p := NewProcessor(config.Config{Store: config.StoreConfig{DataDir: dir}}, "ch", index.NewChannelIndex(index.PublishImmediately), broker)
			p.SetProgress(prog)
			task := videoTask(1, queue.FormatTS)

			for range 2 {
				task.NextRetry = time.Time{}
				broker.Push(task)
				attempt := broker.Pop()
				p.handle(fetch.Result{Task: task, Data: tc.data, Attempt: attempt})
				attempt.Finish()
			} // the same segment again

			if requeued := broker.Len() > 0; requeued != tc.wantRequeue {
				t.Errorf("requeued = %v, want %v", requeued, tc.wantRequeue)
			}
			if got := prog.Snapshot().SegmentsStored; got != tc.wantStored {
				t.Errorf("stored segments counted = %d, want %d", got, tc.wantStored)
			}
		})
	}
}
