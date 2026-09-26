// Package store handles atomic writing of segments to disk.
package store

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/a840817a/sluice/internal/pathseg"
)

// Segment file extensions. DASH and fMP4 HLS segments are stored as .m4s;
// MPEG-TS HLS segments keep their native .ts so they can be served back
// byte-for-byte without any remuxing.
const (
	ExtFMP4 = ".m4s"
	ExtTS   = ".ts"
	// ExtVTT is a WebVTT subtitle segment.
	ExtVTT = ".vtt"
)

// segmentExts is every extension a stored segment can have. ParseSegmentFile
// and SegmentOnDisk both read it, so a new format cannot be recognised by one
// and missed by the other.
var segmentExts = []string{ExtFMP4, ExtTS, ExtVTT}

// SegmentOnDisk reports whether the representation directory dir holds
// segment segNo, in any stored format.
func SegmentOnDisk(dir string, segNo uint64) bool {
	for _, ext := range segmentExts {
		if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d%s", segNo, ext))); err == nil {
			return true
		}
	}
	return false
}

// InitFileName is the on-disk name of a representation's initialization segment.
const InitFileName = "init.mp4"

// ParseSegmentFile parses a stored segment filename such as "123.m4s" or
// "123.ts" into its segment number and extension. It is the single place that
// decides which files in a representation directory are segments, shared by the
// disk rebuilder, the cleanup scanner, and the HTTP segment handler so they can
// never disagree about what is on disk.
func ParseSegmentFile(name string) (segNo uint64, ext string, ok bool) {
	i := strings.LastIndex(name, ".")
	if i <= 0 {
		return 0, "", false
	}
	ext = name[i:]
	if !slices.Contains(segmentExts, ext) {
		return 0, "", false
	}
	segNo, err := strconv.ParseUint(name[:i], 10, 64)
	if err != nil {
		return 0, "", false
	}
	return segNo, ext, true
}

// asDirName returns the directory name that combines mediaType and asID,
// e.g. "video_0", "audio_1", "audio_2".  Using a compound name avoids
// ambiguity between asID ("0") and repID ("720p") when rebuilding from disk.
func asDirName(mediaType, asID string) string {
	return mediaType + "_" + asID
}

// repDir returns the directory that holds one representation's init segment,
// media segments and sidecar.
//
//	{dataDir}/{channelID}/periods/{periodID}/{mediaType}_{asID}/{repID}
//
// The period and representation IDs come from the upstream, so each is written
// as a pathseg-encoded directory name (ADR-0006): an ID with no id attribute
// behind it is empty and would otherwise vanish from the path, and one
// containing "/" or ".." would otherwise place files outside the data
// directory. IDs made of letters, digits, ".", "_" and "-" are unchanged.
// ParseIDDir reads a name back.
func repDir(dataDir, channelID, periodID, mediaType, asID, repID string) string {
	return filepath.Join(
		dataDir,
		channelID,
		"periods",
		pathseg.Encode(periodID),
		asDirName(mediaType, asID),
		pathseg.Encode(repID),
	)
}

// ParseIDDir returns the period or representation ID a directory name under
// "periods" stands for, or false for a name the layout never writes.
func ParseIDDir(name string) (string, bool) { return pathseg.Decode(name) }

// SegmentPath returns the canonical path for an fMP4 media segment.
//
//	{repDir}/{segNo}.m4s
func SegmentPath(dataDir, channelID, periodID, mediaType, asID, repID string, segNo uint64) string {
	return SegmentPathExt(dataDir, channelID, periodID, mediaType, asID, repID, segNo, ExtFMP4)
}

// SegmentPathExt returns the canonical path for a media segment with an
// explicit extension (ExtFMP4 or ExtTS).
func SegmentPathExt(dataDir, channelID, periodID, mediaType, asID, repID string, segNo uint64, ext string) string {
	return filepath.Join(
		repDir(dataDir, channelID, periodID, mediaType, asID, repID),
		fmt.Sprintf("%d%s", segNo, ext),
	)
}

// InitPath returns the canonical path for an init segment.
//
//	{repDir}/init.mp4
func InitPath(dataDir, channelID, periodID, mediaType, asID, repID string) string {
	return filepath.Join(repDir(dataDir, channelID, periodID, mediaType, asID, repID), InitFileName)
}
