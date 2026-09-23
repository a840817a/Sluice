package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteDurableCreatesParentsReplacesDataAndHonorsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state", "checkpoint.json")
	if err := WriteDurable(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("first WriteDurable: %v", err)
	}
	if err := WriteDurable(path, []byte("replacement"), 0o640); err != nil {
		t.Fatalf("replacement WriteDurable: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read durable output: %v", err)
	}
	if !bytes.Equal(got, []byte("replacement")) {
		t.Errorf("durable output = %q, want replacement", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat durable output: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("durable output mode = %o, want 640", info.Mode().Perm())
	}
}
