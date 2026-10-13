// Package cenc removes ISO/IEC 23001-7 Common Encryption from DASH fMP4.
//
// It handles the cenc (AES-CTR) and cbcs (AES-CBC with a pattern) schemes. An
// init segment is rewritten into a clear one; a media segment is decrypted in
// place without changing its size or the offset of any box, so trun data
// offsets, sidx entries and byte ranges into the file all stay valid. See ADR
// 0010.
//
// The package does no I/O and never logs. Content keys never appear in an
// error string or in the formatted form of a Key.
package cenc

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// KID is a 16-byte key ID. It is not secret: it appears in manifests and in
// tenc, and it is what logs and API responses show in place of a key.
type KID [16]byte

// String returns the KID as 32 lowercase hex digits.
func (k KID) String() string { return hex.EncodeToString(k[:]) }

// MarshalText encodes the KID as 32 lowercase hex digits.
func (k KID) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// UnmarshalText decodes 32 hex digits.
func (k *KID) UnmarshalText(b []byte) error {
	v, err := parseHex16(string(b))
	if err != nil {
		return fmt.Errorf("kid: %w", err)
	}
	*k = v
	return nil
}

// Key is one content key and the KID it decrypts.
//
// Its formatted forms (String, GoString, LogValue) show the KID only.
type Key struct {
	KID   KID
	Value [16]byte
}

// KeySet maps a KID to its content key.
type KeySet map[KID][16]byte

// ParseKey parses "kid:key", each 32 hex digits, in either case.
func ParseKey(s string) (Key, error) {
	kidHex, keyHex, ok := strings.Cut(s, ":")
	if !ok {
		return Key{}, errors.New("content key must be kid:key")
	}
	kid, err := parseHex16(kidHex)
	if err != nil {
		return Key{}, fmt.Errorf("content key kid: %w", err)
	}
	val, err := parseHex16(keyHex)
	if err != nil {
		// The message names what is wrong with the key, never its digits.
		return Key{}, fmt.Errorf("content key value: %w", err)
	}
	return Key{KID: kid, Value: val}, nil
}

func parseHex16(s string) ([16]byte, error) {
	var out [16]byte
	if len(s) != 32 {
		return out, fmt.Errorf("want 32 hex digits, got %d characters", len(s))
	}
	if _, err := hex.Decode(out[:], []byte(s)); err != nil {
		return out, errors.New("not hex")
	}
	return out, nil
}

// String shows the KID only.
func (k Key) String() string { return "cenc.Key{KID:" + k.KID.String() + "}" }

// GoString shows the KID only, so %#v does not print the key either.
func (k Key) GoString() string { return k.String() }

// LogValue shows the KID only.
func (k Key) LogValue() slog.Value {
	return slog.GroupValue(slog.String("kid", k.KID.String()))
}
