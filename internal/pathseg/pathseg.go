// Package pathseg encodes an ID an origin chose as one path segment.
//
// Period and representation IDs are arbitrary strings chosen by the origin,
// and the gateway puts them in two kinds of path: the segment URLs it serves
// (ADR-0005) and the directories it stores segments in (ADR-0006). Raw, an ID
// can be empty, which a path join silently drops, or contain "/" and "..",
// which walk out of the directory meant to hold it. Both kinds of path use this
// one encoding so the rules cannot drift apart.
//
// This package must not import anything from internal/: gwurl and store both
// depend on it, and gwurl may import nothing else.
package pathseg

import (
	"encoding/base64"
	"strings"
)

// encodedPrefix marks a segment that carries its ID in base64url. It is not a
// plain-ID character, so no plain ID can be mistaken for an encoded one.
const encodedPrefix = "~"

// Encode returns id as one path segment.
//
// An ID made only of letters, digits, ".", "_" and "-" is used as is, which
// covers generated IDs and the ones DASH packagers normally write, and keeps
// their URLs and directories readable. Anything else — including the empty ID
// a DASH period with no id attribute has — becomes "~" and the ID in unpadded
// base64url, and so do "." and "..", which a path join or a client normalising
// a URL would resolve.
//
// Percent-encoding would be the obvious alternative for URLs and is
// deliberately not used: whether a router hands a handler the escaped or the
// decoded form of a segment depends on how the whole request path happened to
// be escaped, so a percent-encoded ID cannot be decoded reliably. This encoding
// has no escaped form, and it can never contain "$", which opens a DASH
// SegmentTemplate identifier.
func Encode(id string) string {
	if plain(id) {
		return id
	}
	return encodedPrefix + base64.RawURLEncoding.EncodeToString([]byte(id))
}

// Decode reverses Encode. It reports false for a segment Encode never returns,
// including an encoded form of an ID Encode writes as is ("~YQ" for "a"), so
// every ID has exactly one segment and a legacy directory named like that is
// not mistaken for another ID.
func Decode(seg string) (string, bool) {
	if enc, ok := strings.CutPrefix(seg, encodedPrefix); ok {
		id, err := base64.RawURLEncoding.DecodeString(enc)
		return string(id), err == nil && !plain(string(id))
	}
	return seg, plain(seg)
}

func plain(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
