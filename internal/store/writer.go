package store

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write atomically writes data to dest using a temp file in the same directory
// followed by os.Rename (atomic on POSIX filesystems).
//
// The destination directory is created if it doesn't exist.
// Returns the final path on success.
func Write(dest string, data []byte) error {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	// Create temp file in same directory to ensure same filesystem for Rename.
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()

	// Clean up temp file on any error path.
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}

	// Atomic rename: on POSIX this is guaranteed atomic within the same filesystem.
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("rename %s → %s: %w", tmpName, dest, err)
	}

	success = true
	return nil
}
