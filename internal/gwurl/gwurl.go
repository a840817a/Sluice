// Package gwurl builds the URLs and paths this gateway serves under.
//
// It is the single definition of the gateway's public URL shape. Before it
// existed the same path literals were spelled out in the DASH manifest
// generator, the HLS playlist renderer and three HTTP handlers, so changing a
// route meant finding four files and getting the trailing slashes right in each.
//
// The chi route patterns themselves stay literal in internal/httpapi/routes.go:
// a router should be greppable, and httpapi's route table test is what pins the
// patterns and these builders to each other.
//
// This package must not import anything from internal/. It is depended on by
// internal/manifest and internal/hlsout, neither of which may import
// internal/httpapi — that edge is what a shared builder exists to avoid.
package gwurl

import (
	"fmt"
	"strconv"
	"strings"
)

// Base normalizes a configured gateway base URL for joining.
//
// Trailing slashes are stripped because config.yaml examples are written both
// with and without one, and every URL below is built by concatenation. Callers
// that take a base do this themselves; it is exported for the few places that
// need a normalized base without building a URL from it.
func Base(baseURL string) string { return strings.TrimRight(baseURL, "/") }

// --- Relative paths, no leading slash -------------------------------------
//
// These go inside a DASH MPD whose <BaseURL> is already the gateway root, so
// they must stay relative.

// SegmentTemplatePath is the DASH SegmentTemplate media attribute: a
// $Number$ template with nine-digit zero padding.
//
// The padding is deliberate and must not be unified with SegmentURL below. The
// files on disk are not zero-padded — store.SegmentPathExt writes %d — and a
// padded request still resolves because store.ParseSegmentFile parses with
// ParseUint. Collapsing the two forms into one builder would still pass every
// test while changing every HLS segment URI that players and CDNs have cached.
func SegmentTemplatePath(channelID, repID string) string {
	return fmt.Sprintf("v1/channels/%s/segments/%s/$Number%%09d$.m4s", channelID, repID)
}

// InitTemplatePath is the DASH SegmentTemplate initialization attribute.
func InitTemplatePath(channelID, repID string) string {
	return fmt.Sprintf("v1/channels/%s/init/%s.mp4", channelID, repID)
}

// --- Absolute URLs ---------------------------------------------------------
//
// base is a configured gateway base URL; each function normalizes it, so
// callers may pass the raw configured value.

// MediaPlaylistURL is one representation's HLS media playlist.
func MediaPlaylistURL(base, channelID, repID string) string {
	return Base(base) + "/v1/channels/" + channelID + "/media/" + repID + ".m3u8"
}

// SegmentURL is one stored segment, numbered exactly as it is on disk and
// carrying the extension it was stored under (see SegmentTemplatePath for why
// this is not zero-padded).
func SegmentURL(base, channelID, repID string, segNo uint64, ext string) string {
	return Base(base) + "/v1/channels/" + channelID + "/segments/" + repID + "/" +
		strconv.FormatUint(segNo, 10) + ext
}

// InitURL is a representation's fMP4 initialization segment.
func InitURL(base, channelID, repID string) string {
	return Base(base) + "/v1/channels/" + channelID + "/init/" + repID + ".mp4"
}

// KeyProxyBaseURL is the prefix a key ID is appended to, and therefore must
// never end in a slash — KeyURL and the HLS playlist both join with "/".
func KeyProxyBaseURL(base, channelID string) string {
	return Base(base) + "/v1/channels/" + channelID + "/key"
}

// KeyURL is one AES-128 content key, addressed by its published key ID.
func KeyURL(base, channelID, keyID string) string {
	return KeyProxyBaseURL(base, channelID) + "/" + keyID
}

// LicenseURL is a channel's DRM license proxy for one DRM system
// ("playready" or "widevine").
func LicenseURL(base, channelID, drmType string) string {
	return Base(base) + "/v1/channels/" + channelID + "/license/" + drmType
}

// --- Root-relative paths ---------------------------------------------------
//
// For JSON payloads that advertise an endpoint rather than link to it.

// ManifestPath is a channel's DASH manifest, root-relative.
func ManifestPath(channelID string) string {
	return "/v1/channels/" + channelID + "/manifest.mpd"
}

// MasterPath is a channel's HLS master playlist, root-relative.
func MasterPath(channelID string) string {
	return "/v1/channels/" + channelID + "/master.m3u8"
}
