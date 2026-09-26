package httpapi

import (
	"context"
	"path/filepath"
	"testing"

	ch "github.com/a840817a/sluice/internal/channel"
	"github.com/a840817a/sluice/internal/config"
)

// newTestManager returns a Manager over an empty channel store of its own.
func newTestManager(t testing.TB, cfg config.Config) *ch.Manager {
	t.Helper()
	st, err := ch.NewStore(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	return ch.NewManager(cfg, st)
}

// createChannel creates and starts a channel, and returns its runtime.
func createChannel(mgr *ch.Manager, ctx context.Context, id string, cfg ch.Config) (*ch.Runtime, error) {
	if err := mgr.Create(ctx, id, cfg); err != nil {
		return nil, err
	}
	return mgr.Get(id), nil
}
