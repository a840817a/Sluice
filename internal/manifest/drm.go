// Package manifest handles MPD generation and DRM URL rewriting.
package manifest

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// RewriteLAURL rewrites the license acquisition URL inside a base64-encoded
// PlayReady mspr:pro header blob.
//
// The blob is base64(UTF-16LE XML). We decode it, replace the <LA_URL> value,
// and re-encode. Returns the new base64 string.
func RewriteLAURL(msprProB64, newLAURL string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(msprProB64)
	if err != nil {
		return "", fmt.Errorf("mspr:pro base64 decode: %w", err)
	}

	// raw is UTF-16LE encoded XML. Convert to UTF-8 for processing.
	utf8, err := utf16LEToString(raw)
	if err != nil {
		return "", fmt.Errorf("mspr:pro utf16 decode: %w", err)
	}

	// Replace the LA_URL value in the XML string.
	replaced, err := replaceLAURLInXML(utf8, newLAURL)
	if err != nil {
		return "", fmt.Errorf("mspr:pro LA_URL replace: %w", err)
	}

	// Convert back to UTF-16LE and base64 encode.
	utf16, err := stringToUTF16LE(replaced)
	if err != nil {
		return "", fmt.Errorf("mspr:pro utf16 encode: %w", err)
	}
	return base64.StdEncoding.EncodeToString(utf16), nil
}

// replaceLAURLInXML replaces the text content of any <LA_URL> element.
//
// Simple string replacement is deliberate: the XML may use namespaces that a
// full encoding/xml round-trip would not preserve.
func replaceLAURLInXML(xmlStr, newURL string) (string, error) {
	return replaceLAURLInString(xmlStr, newURL), nil
}

// replaceLAURLInString performs a simple string replacement of <LA_URL>...</LA_URL>.
func replaceLAURLInString(s, newURL string) string {
	const open = "<LA_URL>"
	const close = "</LA_URL>"
	start := strings.Index(s, open)
	if start < 0 {
		return s
	}
	end := strings.Index(s[start:], close)
	if end < 0 {
		return s
	}
	end += start
	return s[:start+len(open)] + newURL + s[end:]
}

// utf16LEToString converts a UTF-16LE byte slice to a UTF-8 string.
func utf16LEToString(b []byte) (string, error) {
	if len(b)%2 != 0 {
		return "", fmt.Errorf("odd byte count for UTF-16LE data")
	}
	// Skip BOM if present (0xFF 0xFE)
	start := 0
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		start = 2
	}
	runes := make([]rune, 0, len(b)/2)
	for i := start; i+1 < len(b); i += 2 {
		r := rune(b[i]) | rune(b[i+1])<<8
		runes = append(runes, r)
	}
	return string(runes), nil
}

// stringToUTF16LE converts a UTF-8 string to UTF-16LE bytes (without BOM).
// The original mspr:pro blobs do not carry a BOM, so adding one would corrupt
// the PlayReady Object binary structure whose size field starts at byte 0.
func stringToUTF16LE(s string) ([]byte, error) {
	runes := []rune(s)
	out := make([]byte, len(runes)*2)
	for i, r := range runes {
		// Only handle BMP (U+0000–U+FFFF); surrogate pairs not needed for LA_URL XML
		if r > 0xFFFF {
			return nil, fmt.Errorf("non-BMP character U+%X not supported", r)
		}
		out[i*2] = byte(r)
		out[i*2+1] = byte(r >> 8)
	}
	return out, nil
}
