package cenc

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// assertNoKey fails if s shows the key in hex (either case) or raw.
func assertNoKey(t *testing.T, s string, key [16]byte) {
	t.Helper()
	h := hex.EncodeToString(key[:])
	if strings.Contains(strings.ToLower(s), h) || strings.Contains(s, string(key[:])) {
		t.Errorf("%q reveals the key", s)
	}
}

func TestParseKey(t *testing.T) {
	const kid = "0123456789abcdef0123456789abcdef"
	const val = "fedcba9876543210fedcba9876543210"
	tests := []struct {
		name  string
		in    string
		ok    bool
		wantK string
	}{
		{name: "lowercase", in: kid + ":" + val, ok: true, wantK: kid},
		{name: "uppercase", in: strings.ToUpper(kid + ":" + val), ok: true, wantK: kid},
		{name: "missing colon", in: kid + val},
		{name: "short kid", in: kid[:30] + ":" + val},
		{name: "long key", in: kid + ":" + val + "00"},
		{name: "non-hex kid", in: "zz" + kid[2:] + ":" + val},
		{name: "non-hex key", in: kid + ":" + "zz" + val[2:]},
		{name: "dashed uuid kid", in: "01234567-89ab-cdef-0123-456789abcdef:" + val},
		{name: "two colons", in: kid + ":" + val + ":" + val},
		{name: "empty", in: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, err := ParseKey(tt.in)
			if !tt.ok {
				if err == nil {
					t.Fatalf("ParseKey(%q) = %v, want error", tt.in, k)
				}
				if strings.Contains(strings.ToLower(err.Error()), val[4:]) {
					t.Errorf("error %q echoes the key digits", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseKey: %v", err)
			}
			if k.KID.String() != tt.wantK {
				t.Errorf("KID = %s, want %s", k.KID, tt.wantK)
			}
			if hex.EncodeToString(k.Value[:]) != val {
				t.Errorf("Value = %x, want %s", k.Value, val)
			}
		})
	}
}

// A Key can end up in a log line or an error by accident. Every way of
// formatting it must show the KID and hide the key.
func TestKeyFormattingHidesValue(t *testing.T) {
	k, err := ParseKey("0123456789abcdef0123456789abcdef:fedcba9876543210fedcba9876543210")
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("key", "key", k)
	var jsonLogged bytes.Buffer
	slog.New(slog.NewJSONHandler(&jsonLogged, nil)).Info("key", "key", k)

	outputs := map[string]string{
		"%v":       fmt.Sprintf("%v", k),
		"%+v":      fmt.Sprintf("%+v", k),
		"%#v":      fmt.Sprintf("%#v", k),
		"%s":       fmt.Sprintf("%s", k),
		"pointer":  fmt.Sprintf("%+v", &k),
		"in slice": fmt.Sprintf("%v", []Key{k}),
		"slog":     logged.String(),
		"slogjson": jsonLogged.String(),
	}
	for name, s := range outputs {
		assertNoKey(t, s, k.Value)
		if !strings.Contains(s, k.KID.String()) {
			t.Errorf("%s: %q does not show the KID", name, s)
		}
	}
}
