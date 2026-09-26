package channel

import (
	"context"
	"path/filepath"
	"testing"

	gocfg "github.com/a840817a/sluice/internal/config"
)

// newTestManager returns a Manager over an empty store of its own, kept out of
// the gateway's data directory.
func newTestManager(t *testing.T, gwCfg gocfg.Config) *Manager {
	t.Helper()
	st, err := NewStore(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	return NewManager(gwCfg, st)
}

// startForTest starts a channel the way the gateway's entry points do: its
// configuration is in the store first. A channel already stored keeps its
// stored configuration there, so a test that seeded the store controls what
// a restart reads back. Like start it is idempotent.
func (m *Manager) startForTest(ctx context.Context, id string, cfg Config) (*Runtime, error) {
	defer m.lockOperation(id)()
	if _, ok := m.store.Get(id); !ok {
		if err := m.store.Put(id, cfg); err != nil {
			return nil, err
		}
	}
	return m.start(ctx, id, cfg)
}

// runningStatuses returns the status of every running channel.
func runningStatuses(m *Manager) map[string]ChannelStatus {
	out := map[string]ChannelStatus{}
	for id, c := range m.Channels() {
		if c.Status.Running {
			out[id] = c.Status
		}
	}
	return out
}

// runningStatus returns a channel's status, and false unless it is running.
func runningStatus(m *Manager, id string) (ChannelStatus, bool) {
	c, ok := m.Channel(id)
	return c.Status, ok && c.Status.Running
}
