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

// periodsDirName is the directory under a channel's that holds its periods.
const periodsDirName = "periods"

// ChannelDir is the directory that holds everything the gateway keeps for a
// channel: its periods, and the files that describe and restore it.
func ChannelDir(dataDir, channelID string) string {
	return filepath.Join(dataDir, channelID)
}

// PeriodsDir is the directory that holds every period of a channel's stored
// media.
func PeriodsDir(dataDir, channelID string) string {
	return filepath.Join(ChannelDir(dataDir, channelID), periodsDirName)
}

// OriginMPDFileName is the copy of the origin MPD a period directory keeps.
const OriginMPDFileName = "origin.mpd"

// PeriodOriginMPDPath is where a DASH channel keeps the last origin MPD that
// listed period periodID, so the period can still be described once the
// origin's MPD no longer lists it. ScanChannel skips it, as it skips every
// plain file in a period directory.
func PeriodOriginMPDPath(dataDir, channelID, periodID string) string {
	return filepath.Join(PeriodsDir(dataDir, channelID), pathseg.Encode(periodID), OriginMPDFileName)
}

// asDirName returns the directory name that combines mediaType and asID,
// e.g. "video_0", "audio_1", "audio_2". Using a compound name avoids
// ambiguity between asID ("0") and repID ("720p") when rebuilding from disk.
//
// The AdaptationSet ID is pathseg-encoded like the period and representation
// IDs. Today's IDs are positional numbers, which the encoding leaves as they
// are; encoding them anyway means the directory never depends on that.
func asDirName(mediaType, asID string) string {
	return mediaType + "_" + pathseg.Encode(asID)
}

// parseASDirName splits an AdaptationSet directory name back into its media
// type and ID. It splits at the first "_": a media type (video, audio, text)
// never contains one, while an encoded ID can.
func parseASDirName(name string) (mediaType, asID string, ok bool) {
	mediaType, enc, ok := strings.Cut(name, "_")
	if !ok {
		return "", "", false
	}
	return mediaType, IDFromDirName(enc), true
}

// IDFromDirName returns the ID a directory name stands for. A name the
// encoding never produces is one written before the encoding existed
// (ADR-0006), when the raw ID was the directory name; it is read back as that
// ID so a channel stored then still rebuilds.
func IDFromDirName(name string) string {
	if id, ok := pathseg.Decode(name); ok {
		return id
	}
	return name
}

// RepLocation places one representation's files under the data directory: its
// init segment, media segments and sidecar all live in Dir.
//
//	{DataDir}/{ChannelID}/periods/{PeriodID}/{MediaType}_{ASID}/{RepID}
//
// The period and representation IDs come from the origin, so each is written
// as a pathseg-encoded directory name (ADR-0006): an ID with no id attribute
// behind it is empty and would otherwise vanish from the path, and one
// containing "/" or ".." would otherwise place files outside the data
// directory. IDs made of letters, digits, ".", "_" and "-" are unchanged.
// ScanChannel reads the locations back.
type RepLocation struct {
	DataDir, ChannelID, PeriodID, MediaType, ASID, RepID string
	// scannedDir preserves a legacy, unencoded location found on disk. New
	// writes still use the encoded layout; a scan must read the actual path.
	scannedDir string
}

// Dir is the representation's directory.
func (l RepLocation) Dir() string {
	if l.scannedDir != "" {
		return l.scannedDir
	}
	return filepath.Join(
		PeriodsDir(l.DataDir, l.ChannelID),
		pathseg.Encode(l.PeriodID),
		asDirName(l.MediaType, l.ASID),
		pathseg.Encode(l.RepID),
	)
}

// ASDir is the directory of the representation's AdaptationSet, which holds
// the directories of all its representations.
func (l RepLocation) ASDir() string { return filepath.Dir(l.Dir()) }

// SegmentPath is the path of media segment segNo stored with extension ext
// (ExtFMP4, ExtTS or ExtVTT).
func (l RepLocation) SegmentPath(segNo uint64, ext string) string {
	return filepath.Join(l.Dir(), fmt.Sprintf("%d%s", segNo, ext))
}

// InitPath is the path of the representation's init segment.
func (l RepLocation) InitPath() string { return filepath.Join(l.Dir(), InitFileName) }

// SidecarPath is the path of the representation's segment metadata file.
func (l RepLocation) SidecarPath() string { return filepath.Join(l.Dir(), SidecarFileName) }

// PeriodDir is one stored period of a channel, as ScanChannel finds it.
type PeriodDir struct {
	ID             string
	AdaptationSets []ASDir
}

// ASDir is one stored AdaptationSet of a period, as ScanChannel finds it.
type ASDir struct {
	ID, MediaType string
	Reps          []RepLocation
}

// ScanChannel reads back the locations RepLocation placed under the data
// directory: every period, AdaptationSet and representation directory of
// channelID, with the IDs they were written under. A period or AdaptationSet
// directory with nothing below it is still reported.
//
// A channel with no stored media has no periods directory, and gets nil. A
// subdirectory that cannot be read, a plain file, and an AdaptationSet
// directory whose name the layout never produces are skipped, so one damaged
// directory does not hide the rest of the channel. Entries come in directory
// name order.
func ScanChannel(dataDir, channelID string) ([]PeriodDir, error) {
	base := PeriodsDir(dataDir, channelID)
	periodEntries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var periods []PeriodDir
	for _, pe := range periodEntries {
		if !pe.IsDir() {
			continue
		}
		period := PeriodDir{ID: IDFromDirName(pe.Name())}
		periodPath := filepath.Join(base, pe.Name())
		asEntries, err := os.ReadDir(periodPath)
		if err != nil {
			periods = append(periods, period)
			continue
		}
		for _, ae := range asEntries {
			if !ae.IsDir() {
				continue
			}
			mediaType, asID, ok := parseASDirName(ae.Name())
			if !ok {
				continue
			}
			as := ASDir{ID: asID, MediaType: mediaType}
			asPath := filepath.Join(periodPath, ae.Name())
			repEntries, err := os.ReadDir(asPath)
			if err == nil {
				for _, re := range repEntries {
					if !re.IsDir() {
						continue
					}
					loc := RepLocation{
						DataDir: dataDir, ChannelID: channelID,
						PeriodID: period.ID, MediaType: mediaType, ASID: asID,
						RepID: IDFromDirName(re.Name()),
					}
					if actual := filepath.Join(asPath, re.Name()); actual != loc.Dir() {
						loc.scannedDir = actual
					}
					as.Reps = append(as.Reps, loc)
				}
			}
			period.AdaptationSets = append(period.AdaptationSets, as)
		}
		periods = append(periods, period)
	}
	return periods, nil
}

// RelativeToChannel returns path p relative to channelDir with forward
// slashes, as a playlist or manifest written into channelDir names the files
// beside it. It refuses a path outside channelDir.
func RelativeToChannel(p, channelDir string) (string, error) {
	rel, err := filepath.Rel(channelDir, p)
	if err != nil {
		return "", fmt.Errorf("%q is not under %q: %w", p, channelDir, err)
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("%q escapes the channel directory", p)
	}
	return filepath.ToSlash(rel), nil
}
