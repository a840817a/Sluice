// Package channel manages running channels and their persistent configuration.
//
// The Config DTO and its predicates live in config.go; this file is the store.
package channel

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// Store is a thread-safe persistent store for channel configurations.
// It reads from and writes to a single JSON file.
type Store struct {
	mu       sync.RWMutex
	path     string
	channels map[string]*Config // channelID → config
}

// NewStore loads (or creates) the store at path.
func NewStore(path string) (*Store, error) {
	s := &Store{
		path:     path,
		channels: make(map[string]*Config),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// List returns a snapshot of all channel configs keyed by ID.
func (s *Store) List() map[string]Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Config, len(s.channels))
	for id, cfg := range s.channels {
		out[id] = cloneConfig(*cfg)
	}
	return out
}

// Get returns the config for channelID, and whether it exists.
func (s *Store) Get(id string) (Config, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg, ok := s.channels[id]
	if !ok {
		return Config{}, false
	}
	return cloneConfig(*cfg), true
}

// Put creates or replaces the config for channelID and persists to disk.
func (s *Store) Put(id string, cfg Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := maps.Clone(s.channels)
	cfg = cloneConfig(cfg)
	next[id] = &cfg
	return s.commit(next)
}

// Update applies fn to the stored config for id and persists the result,
// returning the merged config.
//
// It exists because the gateway itself owns a couple of config fields (IsVOD)
// that it writes while an operator may be editing the same channel through the
// admin API. Read-modify-write via Get + Put would drop whichever side lost the
// race; holding the lock across the mutation makes the merge atomic instead.
func (s *Store) Update(id string, fn func(*Config)) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, ok := s.channels[id]
	if !ok {
		return Config{}, fmt.Errorf("channel %q not found", id)
	}
	updated := cloneConfig(*cfg)
	fn(&updated)
	next := maps.Clone(s.channels)
	// The callback may retain its argument or assign caller-owned slices.
	stored := cloneConfig(updated)
	next[id] = &stored
	if err := s.commit(next); err != nil {
		return Config{}, err
	}
	return updated, nil
}

// Delete removes the channel config and persists to disk.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.channels[id]; !ok {
		return fmt.Errorf("channel %q not found", id)
	}
	next := maps.Clone(s.channels)
	delete(next, id)
	return s.commit(next)
}

// cloneConfig keeps mutable fields private to the store, including while an
// Update callback prepares a candidate that may fail to reach disk.
func cloneConfig(cfg Config) Config {
	cfg.FetchHeaders = slices.Clone(cfg.FetchHeaders)
	cfg.LicenseHeaders = slices.Clone(cfg.LicenseHeaders)
	if cfg.EnablePlayReady != nil {
		cfg.EnablePlayReady = BoolPtr(*cfg.EnablePlayReady)
	}
	if cfg.EnableWidevine != nil {
		cfg.EnableWidevine = BoolPtr(*cfg.EnableWidevine)
	}
	if cfg.KeepAllSegments != nil {
		cfg.KeepAllSegments = BoolPtr(*cfg.KeepAllSegments)
	}
	return cfg
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil // fresh start
	}
	if err != nil {
		return fmt.Errorf("channel store read: %w", err)
	}
	return json.Unmarshal(data, &s.channels)
}

// commit persists a candidate before publishing it to readers. The caller
// holds mu throughout so concurrent updates cannot lose each other's changes.
func (s *Store) commit(next map[string]*Config) error {
	if err := s.save(next); err != nil {
		return err
	}
	s.channels = next
	return nil
}

func (s *Store) save(channels map[string]*Config) error {
	data, err := json.MarshalIndent(channels, "", "  ")
	if err != nil {
		return err
	}
	// Atomic write: temp file in same directory then rename.
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".channels-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	defer tmp.Close()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}
