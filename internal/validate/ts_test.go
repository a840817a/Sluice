package validate

import (
	"bytes"
	"testing"
)

// tsPackets builds n well-formed (for our purposes) TS packets.
func tsPackets(n int) []byte {
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		pkt := make([]byte, tsPacketSize)
		pkt[0] = tsSyncByte
		b.Write(pkt)
	}
	return b.Bytes()
}

func TestValidateTSAcceptsWellFormed(t *testing.T) {
	for _, n := range []int{1, 2, 3, 10} {
		if err := ValidateTS(tsPackets(n)); err != nil {
			t.Errorf("ValidateTS(%d packets) = %v, want nil", n, err)
		}
	}
}

func TestValidateTSRejectsBadInput(t *testing.T) {
	html := []byte("<html><body>404 Not Found</body></html>")

	misaligned := tsPackets(2)
	misaligned = misaligned[:len(misaligned)-1]

	badSync := tsPackets(3)
	badSync[2*tsPacketSize] = 0x00 // third packet loses its sync byte

	tests := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"origin error page", html},
		{"not a packet multiple", misaligned},
		{"sync byte missing mid-stream", badSync},
	}
	for _, tc := range tests {
		if err := ValidateTS(tc.data); err == nil {
			t.Errorf("ValidateTS(%s) = nil, want error", tc.name)
		}
	}
}

func TestValidateTSEncryptedIgnoresSyncBytes(t *testing.T) {
	// Ciphertext looks like noise. Rejecting it for a missing sync byte is the
	// exact bug this separate entry point exists to prevent.
	ciphertext := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 4*16)
	if err := ValidateTSEncrypted(ciphertext); err != nil {
		t.Errorf("ValidateTSEncrypted(ciphertext) = %v, want nil", err)
	}
	if err := ValidateTS(ciphertext); err == nil {
		t.Error("ValidateTS accepted ciphertext; the two entry points must differ")
	}
}

func TestValidateTSEncryptedRejectsPartialBlock(t *testing.T) {
	if err := ValidateTSEncrypted(make([]byte, 17)); err == nil {
		t.Error("ValidateTSEncrypted(17 bytes) = nil, want error")
	}
	if err := ValidateTSEncrypted(nil); err == nil {
		t.Error("ValidateTSEncrypted(nil) = nil, want error")
	}
}
