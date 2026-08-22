package httpapi

// Serving stored media: segments, init sections, and the index lookups that
// resolve a request path to a file this gateway has actually published.

import (
	"net/http"
	"os"
	"strings"

	"github.com/a840817a/sluice/internal/index"
	"github.com/a840817a/sluice/internal/store"
	"github.com/go-chi/chi/v5"
)

// handleSegment serves a media segment from disk.
// It first looks up the Published path in the index; only segments that have
// passed the A/V barrier are served. Unknown segment numbers return 404.
//
// GET /v1/channels/{channelID}/segments/{repID}/{segFile}
func (s *Server) handleSegment(w http.ResponseWriter, r *http.Request) {
	channelID := chi.URLParam(r, "channelID")
	repID := chi.URLParam(r, "repID")
	segFile := chi.URLParam(r, "segFile") // e.g. "000000001.m4s"

	rt := s.manager.Get(channelID)
	if rt == nil {
		http.NotFound(w, r)
		return
	}

	// Parse segment number from filename.
	segNo, ok := parseSegNo(segFile)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// Look up Published path in the index (VOD mode uses full-history index).
	if path := publishedSegPath(rt.ActiveIndex(), repID, segNo); path != "" {
		serveFile(w, r, path, segmentContentType(path))
		return
	}
	http.NotFound(w, r)
}

// handleInit serves the init segment for a representation.
// The init path is taken from any Published (or Committed) segment in the index.
//
// GET /v1/channels/{channelID}/init/{repID}.mp4
func (s *Server) handleInit(w http.ResponseWriter, r *http.Request) {
	channelID := chi.URLParam(r, "channelID")
	repID := chi.URLParam(r, "repID")

	rt := s.manager.Get(channelID)
	if rt == nil {
		http.NotFound(w, r)
		return
	}

	if path := publishedInitPath(rt.ActiveIndex(), repID); path != "" {
		serveFile(w, r, path, "video/mp4")
		return
	}
	http.NotFound(w, r)
}

func serveFile(w http.ResponseWriter, r *http.Request, path, contentType string) {
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		http.Error(w, "stat error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeContent(w, r, stat.Name(), stat.ModTime(), f)
}

// parseSegNo parses the segment number from a filename like "000000001.m4s"
// or "000000001.ts".
func parseSegNo(segFile string) (uint64, bool) {
	n, _, ok := store.ParseSegmentFile(segFile)
	return n, ok
}

// segmentContentType picks the media type from the extension of the file that
// was actually resolved on disk, rather than from the requested name, so the
// stored format is always what determines the header.
func segmentContentType(path string) string {
	switch {
	case strings.HasSuffix(path, store.ExtTS):
		return "video/mp2t"
	case strings.HasSuffix(path, store.ExtVTT):
		return "text/vtt"
	default:
		return "video/mp4"
	}
}

// publishedSegPath searches all periods in ci for a Published segment matching
// repID and segNo, returning its on-disk path. Returns "" if not found.
func publishedSegPath(ci *index.ChannelIndex, repID string, segNo uint64) string {
	var path string
	ci.ForEachRepUntil(func(ref index.RepRef, rep *index.RepresentationState) bool {
		if ref.RepID != repID {
			return true
		}
		for _, seg := range rep.Published() {
			if seg.SegNo == segNo {
				path = seg.Path
				return false
			}
		}
		return true
	})
	return path
}

// publishedInitPath returns the InitPath from any Published segment for repID.
// Falls back to Committed if no Published segment exists yet (covers init-only requests).
func publishedInitPath(ci *index.ChannelIndex, repID string) string {
	var published, committed string
	ci.ForEachRepUntil(func(ref index.RepRef, rep *index.RepresentationState) bool {
		if ref.RepID != repID {
			return true
		}
		for _, seg := range rep.Published() {
			if seg.InitPath != "" {
				published = seg.InitPath
				return false
			}
		}
		for _, seg := range rep.Committed() {
			if seg.InitPath != "" {
				committed = seg.InitPath
			}
		}
		return true
	})
	if published != "" {
		return published
	}
	return committed
}
