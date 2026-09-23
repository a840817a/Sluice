package store

import (
	"fmt"
	"os"
	"path/filepath"
)

// SyncDir persists directory entries after a rename. File Sync alone does not
// make the replacement name durable.
func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// WriteDurable atomically replaces a file, then syncs its directory and any
// directories created for it. An error after rename may leave the replacement
// visible; callers must allow an idempotent retry rather than assume rollback.
func WriteDurable(dest string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(dest)
	parent := dir
	for {
		_, err := os.Stat(parent)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return err
		}
		parent = next
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(f.Name(), dest); err != nil {
		return fmt.Errorf("rename %s → %s: %w", f.Name(), dest, err)
	}
	for current := dir; ; current = filepath.Dir(current) {
		if err := SyncDir(current); err != nil {
			return fmt.Errorf("sync directory %s: %w", current, err)
		}
		if current == parent {
			return nil
		}
	}
}
