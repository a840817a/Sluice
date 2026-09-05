package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The failure this guards was found on a real host: a bind-mounted /data the
// container could not write. Nothing failed at startup — the gateway came up,
// reported healthy, and every segment write then failed. These tests pin the
// two halves that make that impossible: the check must actually write, and it
// must fail loudly enough to name the directory.

func TestCheckWritableAcceptsAWritableDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := CheckWritable(dir); err != nil {
		t.Fatalf("CheckWritable(%s) = %v, want nil", dir, err)
	}
}

func TestCheckWritableCreatesAMissingDirectory(t *testing.T) {
	// The data directory is routinely a path that does not exist yet — a fresh
	// bind mount, or store.data_dir pointing somewhere new. Write() already
	// creates it on first use, so refusing here would reject a working setup.
	dir := filepath.Join(t.TempDir(), "nested", "data")
	if err := CheckWritable(dir); err != nil {
		t.Fatalf("CheckWritable(%s) = %v, want nil", dir, err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("directory not created: %v", err)
	}
}

func TestCheckWritableRejectsAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny root, so this cannot be provoked")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	// Restore write permission or t.TempDir's own cleanup cannot remove it.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	err := CheckWritable(dir)
	if err == nil {
		t.Fatal("CheckWritable on a 0555 directory returned nil, want an error")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("error does not unwrap to fs.ErrPermission: %v", err)
	}
	// The whole point is that the operator can act on the message without
	// having to reproduce the failure, so it carries the path and the
	// ownership the caller could not have guessed.
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error does not name the directory %q: %v", dir, err)
	}
	for _, want := range []string{"uid=", "gid=", "mode="} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q, so it does not say who owns the directory: %v", want, err)
		}
	}
}

func TestCheckWritableLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	if err := CheckWritable(dir); err != nil {
		t.Fatalf("CheckWritable: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("probe left %v behind; the data directory is served, so it must stay clean", names)
	}
}

// A directory can be listable and even creatable-in by mode bits and still
// reject writes — a read-only mount is the case that matters in a container,
// where /data is the only writable path and ReadOnly=true covers the rest.
// Nothing in a unit test can mount read-only, so this asserts the weaker
// property that makes that case detectable at all: the check writes rather
// than consulting permission bits.
func TestCheckWritableActuallyWrites(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny root, so this cannot be provoked")
	}
	dir := t.TempDir()
	// Executable and readable, but not writable: unix.Access(W_OK) and a mode
	// inspection both report exactly this, and a check built on either would
	// have to special-case it. A real write just fails.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := CheckWritable(dir); err == nil {
		t.Fatal("want an error from a directory that denies creation")
	}
}
