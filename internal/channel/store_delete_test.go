package channel

import (
	"path/filepath"
	"testing"
)

func storeWith(t *testing.T, ids ...string) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "channels.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	for _, id := range ids {
		if err := s.Put(id, Config{MPDURL: "http://origin/" + id + ".mpd", Enabled: true}); err != nil {
			t.Fatalf("Put %s: %v", id, err)
		}
	}
	return s, path
}

// Delete is the only way a channel leaves channels.json, and that file is what
// the gateway restores from on the next start. A delete that does not reach disk
// resurrects the channel at the next restart.
func TestStoreDeletePersists(t *testing.T) {
	s, path := storeWith(t, "keep", "drop")

	if err := s.Delete("drop"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, ok := s.Get("drop"); ok {
		t.Error("deleted channel is still readable from the store")
	}
	if _, ok := s.Get("keep"); !ok {
		t.Error("Delete removed a channel it was not asked about")
	}

	// What a restart would see.
	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := reloaded.Get("drop"); ok {
		t.Error("deleted channel came back after reloading channels.json")
	}
	if _, ok := reloaded.Get("keep"); !ok {
		t.Error("the surviving channel did not reload")
	}
}

// Deleting something that was never there is the caller's mistake, not a silent
// success: the admin handler turns this into a 404 rather than reporting that it
// removed something.
func TestStoreDeleteUnknownChannelErrors(t *testing.T) {
	s, path := storeWith(t, "keep")

	err := s.Delete("never-existed")
	if err == nil {
		t.Fatal("Delete of an unknown channel succeeded, want an error")
	}

	reloaded, err2 := NewStore(path)
	if err2 != nil {
		t.Fatalf("reload: %v", err2)
	}
	if _, ok := reloaded.Get("keep"); !ok {
		t.Error("a failed delete disturbed the stored channels")
	}
}

// Deleting the last channel must leave a valid empty store, not a file that
// fails to parse on the next start.
func TestStoreDeleteLastChannelLeavesALoadableFile(t *testing.T) {
	s, path := storeWith(t, "only")

	if err := s.Delete("only"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatalf("reload after deleting the last channel: %v", err)
	}
	if n := len(reloaded.List()); n != 0 {
		t.Errorf("reloaded store has %d channels, want 0", n)
	}
}
