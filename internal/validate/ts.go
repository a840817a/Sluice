package validate

import (
	"errors"
	"fmt"
)

// tsPacketSize is the fixed MPEG-TS packet length (ISO/IEC 13818-1).
const tsPacketSize = 188

// tsSyncByte begins every MPEG-TS packet.
const tsSyncByte = 0x47

// ValidateTS performs a structural sanity check on an MPEG-TS segment.
//
// Unlike fMP4 there is no box structure to walk and no timing to extract: the
// segment's presentation time comes from the playlist's EXTINF durations, not
// from the bytes. So this checks only that the payload really is a TS stream —
// a whole number of 188-byte packets, each starting with the sync byte. That is
// enough to catch the failure that actually happens in practice, which is an
// origin returning an HTML error page with a 200 status.
//
// This must never be called on ciphertext. Under AES-128 passthrough the bytes
// are encrypted and the sync-byte check would reject every valid segment; use
// ValidateTSEncrypted for that case.
func ValidateTS(data []byte) error {
	if len(data) == 0 {
		return errors.New("empty TS segment")
	}
	if len(data)%tsPacketSize != 0 {
		return fmt.Errorf("TS segment length %d is not a multiple of %d", len(data), tsPacketSize)
	}
	// Checking the first few packet boundaries is enough to reject non-TS
	// payloads without paying to scan a multi-megabyte segment.
	for off := 0; off < len(data) && off <= 2*tsPacketSize; off += tsPacketSize {
		if data[off] != tsSyncByte {
			return fmt.Errorf("missing TS sync byte at offset %d (got 0x%02x)", off, data[off])
		}
	}
	return nil
}

// ValidateTSEncrypted checks an AES-128 encrypted TS segment. Only the length
// is verifiable: the payload is CBC ciphertext, so it must be a whole number of
// AES blocks, and nothing about its contents can be inspected without the key.
func ValidateTSEncrypted(data []byte) error {
	const aesBlockSize = 16
	if len(data) == 0 {
		return errors.New("empty encrypted TS segment")
	}
	if len(data)%aesBlockSize != 0 {
		return fmt.Errorf("encrypted TS segment length %d is not a multiple of the AES block size", len(data))
	}
	return nil
}
