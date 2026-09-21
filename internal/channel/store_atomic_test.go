package channel

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStoreSaveFailureKeepsCommittedState(t *testing.T) {
	operations := map[string]func(*Store) error{
		"create":  func(s *Store) error { return s.Put("new", Config{Title: "new"}) },
		"replace": func(s *Store) error { return s.Put("keep", Config{Title: "replacement"}) },
		"update": func(s *Store) error {
			_, err := s.Update("keep", func(c *Config) {
				c.Title = "updated"
				c.FetchHeaders[0].Value = "changed"
				c.LicenseHeaders[0].Value = "changed"
				*c.EnablePlayReady = false
				*c.EnableWidevine = false
				*c.KeepAllSegments = false
			})
			return err
		},
		"delete": func(s *Store) error { return s.Delete("keep") },
	}
	for name, operation := range operations {
		for _, stage := range []string{"create-temp", "rename"} {
			t.Run(name+"/"+stage, func(t *testing.T) {
				s, path := storeWith(t, "keep", "other")
				cfg := Config{
					Title: "original", FetchHeaders: []Header{{Name: "X-Test", Value: "original"}},
					LicenseHeaders:  []Header{{Name: "X-Test", Value: "original"}},
					EnablePlayReady: BoolPtr(true), EnableWidevine: BoolPtr(true), KeepAllSegments: BoolPtr(true),
				}
				if err := s.Put("keep", cfg); err != nil {
					t.Fatal(err)
				}
				// A separately loaded store gives us an independent snapshot, including pointers and slices.
				before, err := NewStore(path)
				if err != nil {
					t.Fatal(err)
				}
				want := before.List()
				blocked := filepath.Join(filepath.Dir(path), "blocked")
				if stage == "create-temp" {
					if err := os.WriteFile(blocked, nil, 0o600); err != nil {
						t.Fatal(err)
					}
					s.path = filepath.Join(blocked, "channels.json")
				} else {
					if err := os.Mkdir(blocked, 0o700); err != nil {
						t.Fatal(err)
					}
					s.path = blocked // Renaming a regular file over a directory must fail.
				}
				if err := operation(s); err == nil {
					t.Fatal("save unexpectedly succeeded")
				}
				if got := s.List(); !reflect.DeepEqual(got, want) {
					t.Error("failed save changed in-memory configuration")
				}
				for id, expected := range want {
					if got, ok := s.Get(id); !ok || !reflect.DeepEqual(got, expected) {
						t.Errorf("Get(%q) changed after failed save", id)
					}
				}
				leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".channels-*.json"))
				if err != nil {
					t.Fatal(err)
				}
				if len(leftovers) != 0 {
					t.Errorf("temporary files left after failure: %v", leftovers)
				}
				disk, err := NewStore(path)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(disk.List(), want) {
					t.Error("failed save changed the last committed file")
				}
				// A later successful save must not accidentally persist the failed mutation.
				s.path = path
				if err := s.Put("other", want["other"]); err != nil {
					t.Fatal(err)
				}
				reloaded, err := NewStore(path)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(reloaded.List(), want) {
					t.Error("failed mutation persisted by a later save")
				}
				// The original operation remains retryable after storage recovers.
				if err := operation(s); err != nil {
					t.Fatal(err)
				}
				reloaded, err = NewStore(path)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(reloaded.List(), s.List()) {
					t.Error("successful retry differs from disk")
				}
			})
		}
	}
}

func TestStoreConfigOwnership(t *testing.T) {
	for _, boundary := range []string{"put", "get", "list", "update-result", "update-callback"} {
		t.Run(boundary, func(t *testing.T) {
			s, path := storeWith(t, "keep")
			cfg := Config{
				FetchHeaders:    []Header{{Name: "X-Test", Value: "original"}},
				LicenseHeaders:  []Header{{Name: "X-Test", Value: "original"}},
				EnablePlayReady: BoolPtr(true), EnableWidevine: BoolPtr(true), KeepAllSegments: BoolPtr(true),
			}
			if err := s.Put("keep", cfg); err != nil {
				t.Fatal(err)
			}
			var external *Config
			switch boundary {
			case "put":
				external = &cfg
			case "get":
				got, _ := s.Get("keep")
				external = &got
			case "list":
				got := s.List()["keep"]
				external = &got
			default:
				result, err := s.Update("keep", func(c *Config) { external = c })
				if err != nil {
					t.Fatal(err)
				}
				if boundary == "update-result" {
					external = &result
				}
			}
			external.FetchHeaders[0].Value = "changed"
			external.LicenseHeaders[0].Value = "changed"
			*external.EnablePlayReady = false
			*external.EnableWidevine = false
			*external.KeepAllSegments = false
			disk, err := NewStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(s.List(), disk.List()) {
				t.Error("caller-owned configuration mutated committed store state")
			}
		})
	}
}
