package validate

import (
	"bytes"
	"errors"
)

// ValidateWebVTT checks that a subtitle segment is a WebVTT payload. WebVTT
// files begin with the "WEBVTT" signature (RFC 8216 §3.5 / the WebVTT spec),
// optionally preceded by a UTF-8 BOM. This is enough to reject an origin error
// page served with a 200, without parsing cues.
func ValidateWebVTT(data []byte) error {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM
	if !bytes.HasPrefix(data, []byte("WEBVTT")) {
		return errors.New("not a WebVTT segment: missing WEBVTT signature")
	}
	return nil
}
