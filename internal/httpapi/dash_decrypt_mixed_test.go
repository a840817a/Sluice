package httpapi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	mp4 "github.com/abema/go-mp4"

	"github.com/a840817a/sluice/internal/dashdecrypt"
)

// mixedKID is the KID of representation "s", for which no key is held.
const mixedKID = "40404040404040404040404040404040"

// singleRep is "s": the video under mixedKID, packaged as one SegmentBase
// file (init, sidx, then the four segments) at s/file.mp4.
var singleRep = cencRep{id: "s", dir: "video/avc1", segments: 4}

// serveSegmentBaseVideo has o serve s's file and lists s in the video
// AdaptationSet beside v, whose SegmentTemplate moves into v so that s
// does not inherit it.
func serveSegmentBaseVideo(t *testing.T, o *cencOrigin) {
	t.Helper()
	video := cencReps[0]
	init := o.fixture(t, "cenc", video, "init.mp4")
	kid := bytes.Index(init, []byte("tenc")) + 4 + 4 + 4
	if kid < 12 || !bytes.Equal(init[kid:kid+16], bytes.Repeat([]byte{0x10}, 16)) {
		t.Fatal("video init's tenc carries no video KID to rewrite")
	}
	copy(init[kid:], bytes.Repeat([]byte{0x40}, 16))

	var segs [][]byte
	refs := make([]mp4.SidxReference, 0, singleRep.segments)
	for i := 1; i <= singleRep.segments; i++ {
		seg := o.fixture(t, "cenc", video, fmt.Sprintf("seg-%d.m4s", i))
		segs = append(segs, seg)
		refs = append(refs, mp4.SidxReference{
			ReferencedSize: uint32(len(seg)), SubsegmentDuration: 6144, StartsWithSAP: true, SAPType: 1,
		})
	}
	var payload bytes.Buffer
	sidx := &mp4.Sidx{ReferenceID: 1, Timescale: 12288, ReferenceCount: uint16(len(refs)), References: refs}
	if _, err := mp4.Marshal(&payload, sidx, mp4.Context{}); err != nil {
		t.Fatalf("marshal sidx: %v", err)
	}
	file := slices.Clone(init)
	idxStart := len(file)
	file = binary.BigEndian.AppendUint32(file, uint32(8+payload.Len()))
	file = append(file, "sidx"...)
	file = append(file, payload.Bytes()...)
	idxEnd := len(file) - 1
	for _, seg := range segs {
		file = append(file, seg...)
	}
	o.extra.Store("s/file.mp4", file)

	o.setEdit(func(mpd string) string {
		video, rest, ok := strings.Cut(mpd, "<!-- Audio -->")
		start := strings.Index(video, "<SegmentTemplate")
		const end = "</SegmentTemplate>"
		stop := strings.Index(video, end)
		rep := `<Representation id="v" codecs="avc1.42D00B" width="160" height="90" scanType="progressive" frameRate="24" bandwidth="43120"/>`
		if !ok || start < 0 || stop < start || !strings.Contains(video, rep) {
			panic("video AdaptationSet of the fixture MPD not as expected")
		}
		tmpl := video[start : stop+len(end)]
		video = video[:start] + video[stop+len(end):]
		video = strings.Replace(video, rep, strings.TrimSuffix(rep, "/>")+">"+tmpl+"</Representation>\n"+
			fmt.Sprintf(`<Representation id="s" codecs="avc1.42D00B" width="160" height="90" bandwidth="40000">`+
				`<BaseURL>s/file.mp4</BaseURL><SegmentBase timescale="12288" indexRange="%d-%d">`+
				`<Initialization range="0-%d"/></SegmentBase></Representation>`, idxStart, idxEnd, len(init)-1), 1)
		return video + "<!-- Audio -->" + rest
	})
}

// An AdaptationSet whose representations use different segment addressing
// is decided from all of them: the SegmentTemplate one holds a key but the
// SegmentBase one does not, so the whole AdaptationSet stays ciphertext with
// a missing_key warning, and neither representation is hidden.
func TestDASHDecryptMixedAddressingKeepsAdaptationSetEncrypted(t *testing.T) {
	o := startCENCOrigin(t, "cenc", nil)
	serveSegmentBaseVideo(t, o)
	// s's init is served only once v's init is stored, or after a second:
	// v, which holds its key, must not be the first to decide the
	// AdaptationSet merely because s is slow. Requests for s's index pass.
	release := make(chan struct{})
	inner := o.srv.Config.Handler
	o.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/s/file.mp4" && strings.HasPrefix(r.Header.Get("Range"), "bytes=0-") {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		inner.ServeHTTP(w, r)
	})
	mgr, h := newCENCChannel(t, o, t.TempDir(), cencVideoKey, cencAudioKey)
	go func() {
		defer close(release)
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if code, _ := get(t, h, initURL(cencReps[0])); code == http.StatusOK {
				return
			}
		}
	}()
	waitVOD(t, mgr)

	video := cencReps[0]
	if code, init := get(t, h, initURL(video)); code != http.StatusOK || !bytes.Equal(init, o.fixture(t, "cenc", video, "init.mp4")) {
		t.Errorf("v init: %d, want 200 with the origin's ciphertext", code)
	}
	for _, rep := range []cencRep{video, singleRep} {
		for i := 1; i <= rep.segments; i++ {
			want := o.fixture(t, "cenc", video, fmt.Sprintf("seg-%d.m4s", i))
			if code, seg := get(t, h, segmentURL(rep, i)); code != http.StatusOK || !bytes.Equal(seg, want) {
				t.Errorf("%s segment %d: %d, want 200 with the origin's ciphertext", rep.id, i, code)
			}
		}
	}
	if code, _ := get(t, h, initURL(singleRep)); code != http.StatusOK {
		t.Errorf("s init: %d, want 200", code)
	}
	assertDecrypted(t, h, o, cencReps[1], cencReps[1].segments)

	code, body := get(t, h, "/v1/channels/c/manifest.mpd")
	videoAS, _, _ := strings.Cut(string(body), `mimeType="audio/mp4"`)
	if code != http.StatusOK || !strings.Contains(videoAS, `id="v"`) || !strings.Contains(videoAS, `id="s"`) ||
		!strings.Contains(videoAS, "ContentProtection") {
		t.Errorf("manifest: %d, want the video AdaptationSet protected and listing v and s:\n%s", code, body)
	}

	ws := decryptWarnings(t, mgr)
	if !slices.ContainsFunc(ws, func(w dashdecrypt.Warning) bool {
		return w.Kind == dashdecrypt.KindMissingKey && w.AdaptationSet == "0" && slices.Equal(w.KIDs, []string{mixedKID})
	}) {
		t.Errorf("decrypt warnings = %+v, want missing_key for s's KID in AdaptationSet 0", ws)
	}
}
