package channel

// Operator configuration changes, coordinated with the channel lifecycle.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

var (
	ErrChannelNotFound = errors.New("channel not found")
	ErrChannelExists   = errors.New("channel already exists")
)

// UpdateResult describes the lifecycle work UpdateConfig performed.
type UpdateResult struct {
	Restarted     bool
	RestartFields []string
}

func (m *Manager) storeForOperation() (*Store, error) {
	m.mu.RLock()
	store := m.store
	m.mu.RUnlock()
	if store == nil {
		return nil, fmt.Errorf("no channel store attached")
	}
	return store, nil
}

// mergedOperatorConfig replaces the fields an operator owns while retaining
// gateway-owned IsVOD at the instant Store.Update commits. RestoreToLive is
// the sole operation allowed to force IsVOD back to false.
func mergedOperatorConfig(requested, current Config, restoreLive bool) Config {
	next := cloneConfig(requested)
	next.Enabled = true
	if !restoreLive {
		next.IsVOD = current.IsVOD
	}
	return next
}

// restartWithConfig stops, joins, then re-reads and persists the configuration
// before starting. Stop may join a VOD transition, so reading before Stop
// would allow an old IsVOD=false snapshot to overwrite its completed state.
// operationMu must be held.
func (m *Manager) restartWithConfig(ctx context.Context, id string, requested Config, restoreLive bool) error {
	store, err := m.storeForOperation()
	if err != nil {
		return err
	}

	_ = m.stop(id)
	oldCfg, ok := store.Get(id)
	if !ok {
		return ErrChannelNotFound
	}

	merged, err := store.Update(id, func(current *Config) {
		*current = mergedOperatorConfig(requested, *current, restoreLive)
	})
	if err != nil {
		if oldCfg.Enabled {
			if _, rollbackErr := m.start(ctx, id, oldCfg); rollbackErr != nil {
				return fmt.Errorf("store error: %w; rollback start error: %w", err, rollbackErr)
			}
		}
		return fmt.Errorf("store error: %w", err)
	}
	if _, err := m.start(ctx, id, merged); err != nil {
		rollbackErr := store.Put(id, oldCfg)
		var rollbackStartErr error
		if oldCfg.Enabled {
			_, rollbackStartErr = m.start(ctx, id, oldCfg)
		}
		if rollbackErr != nil || rollbackStartErr != nil {
			return fmt.Errorf("start error: %w; rollback error: %w", err, errors.Join(rollbackErr, rollbackStartErr))
		}
		return fmt.Errorf("start error: %w", err)
	}
	return nil
}

// RestoreToLive brings a channel that has transitioned to VOD back to live
// ingest: it clears IsVOD, restarts the channel, and marks it so the automatic
// VOD transition does not immediately undo the restore.
//
// Nothing on disk is touched. The segments recorded so far stay where they are
// and the live index rebuilds from them, so a keep-all channel resumes with its
// whole history intact. A channel that explicitly asked for the sliding window
// comes back with only a window's worth of it — and, under
// store.cleanup: on_expire, will start deleting what falls out. That is the
// operator's call to make; the admin UI warns before asking for it.
func (m *Manager) RestoreToLive(ctx context.Context, id string) error {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	store, err := m.storeForOperation()
	if err != nil {
		return err
	}

	cfg, ok := store.Get(id)
	if !ok {
		return fmt.Errorf("channel %q not found", id)
	}
	if !cfg.IsVOD {
		return fmt.Errorf("channel %q is not in VOD mode", id)
	}

	next := cfg
	next.IsVOD = false

	m.mu.Lock()
	m.suppressVOD[id] = true
	m.mu.Unlock()

	if err := m.restartWithConfig(ctx, id, next, true); err != nil {
		// The rollback restarted the channel as VOD, which never consults the
		// mark — but leaving it set would suppress the transition of whatever
		// starts this channel next.
		m.mu.Lock()
		delete(m.suppressVOD, id)
		m.mu.Unlock()
		return err
	}
	slog.Info("channel restored to live", "id", id)
	return nil
}

// UpdateConfig commits an operator edit and applies it without interruption
// when possible. It is the sole owner of the persist/apply/restart protocol.
func (m *Manager) UpdateConfig(ctx context.Context, id string, requested Config) (UpdateResult, error) {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()

	store, err := m.storeForOperation()
	if err != nil {
		return UpdateResult{}, err
	}
	if _, ok := store.Get(id); !ok {
		return UpdateResult{}, ErrChannelNotFound
	}

	m.mu.RLock()
	rt, ok := m.runtimes[id]
	m.mu.RUnlock()
	if !ok {
		err := m.restartWithConfig(ctx, id, requested, false)
		return UpdateResult{Restarted: true}, err
	}

	rt.configMu.Lock()
	oldCfg, exists := store.Get(id)
	if !exists {
		rt.configMu.Unlock()
		return UpdateResult{}, ErrChannelNotFound
	}
	next := mergedOperatorConfig(requested, oldCfg, false)
	blockers := RestartRequired(rt.Config(), next, rt.Mode())
	if len(blockers) > 0 {
		rt.configMu.Unlock()
		err := m.restartWithConfig(ctx, id, requested, false)
		return UpdateResult{Restarted: true, RestartFields: blockers}, err
	}

	merged, err := store.Update(id, func(current *Config) {
		*current = mergedOperatorConfig(requested, *current, false)
	})
	if err != nil {
		rt.configMu.Unlock()
		return UpdateResult{}, fmt.Errorf("store error: %w", err)
	}
	// Persist first. A failed write therefore cannot leave a runtime carrying a
	// configuration that will disappear at the next gateway restart.
	rt.headers.Store(HeaderMap(merged.FetchHeaders))
	rt.setConfig(merged)
	rt.configMu.Unlock()
	slog.Info("channel config hot-applied", "id", id)
	return UpdateResult{}, nil
}

// Create persists and starts a new channel as one lifecycle operation.
func (m *Manager) Create(ctx context.Context, id string, cfg Config) error {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	store, err := m.storeForOperation()
	if err != nil {
		return err
	}
	if _, ok := store.Get(id); ok {
		return ErrChannelExists
	}
	if err := store.Put(id, cfg); err != nil {
		return fmt.Errorf("store error: %w", err)
	}
	if _, err := m.start(ctx, id, cfg); err != nil {
		return fmt.Errorf("start error: %w", err)
	}
	return nil
}

// Restart stops and starts a channel from the configuration current at the
// end of Stop. That read includes any VOD transition Stop had to join.
func (m *Manager) Restart(ctx context.Context, id string) error {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	store, err := m.storeForOperation()
	if err != nil {
		return err
	}
	if _, ok := store.Get(id); !ok {
		return ErrChannelNotFound
	}
	_ = m.stop(id)
	cfg, ok := store.Get(id)
	if !ok {
		return ErrChannelNotFound
	}
	if _, err := m.start(ctx, id, cfg); err != nil {
		return fmt.Errorf("start error: %w", err)
	}
	return nil
}

// Delete stops a channel before removing its configuration. Segment data is
// intentionally left in place.
func (m *Manager) Delete(id string) error {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	store, err := m.storeForOperation()
	if err != nil {
		return err
	}
	if _, ok := store.Get(id); !ok {
		return ErrChannelNotFound
	}
	_ = m.stop(id)
	if err := store.Delete(id); err != nil {
		return fmt.Errorf("store error: %w", err)
	}
	return nil
}
