package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The repo's own config.yaml.example must parse, and segment_timeout must
// survive the round trip — a knob nobody can set from the file is no knob.
func TestSegmentTimeoutParsesFromYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte("worker:\n  segment_timeout: \"120s\"\n"), 0o644)

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Worker.SegmentTimeout != 120*time.Second {
		t.Errorf("segment_timeout = %v, want 2m", cfg.Worker.SegmentTimeout)
	}

	// Omitted must fall back to 30s, so existing config files are unaffected.
	p2 := filepath.Join(dir, "d.yaml")
	os.WriteFile(p2, []byte("worker:\n  fetch_workers: 2\n"), 0o644)
	cfg2, err := Load(p2)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg2.Worker.SegmentTimeout != 30*time.Second {
		t.Errorf("default segment_timeout = %v, want 30s", cfg2.Worker.SegmentTimeout)
	}
}
