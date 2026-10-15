package channel

// Adding content keys to a stored channel.

import (
	"context"
	"errors"
	"fmt"

	"github.com/a840817a/sluice/internal/cenc"
	"github.com/a840817a/sluice/internal/queue"
)

var (
	// ErrKeysNotApplicable: the channel is not a DASH source in decrypt mode.
	ErrKeysNotApplicable = errors.New("content keys can only be added to a dash channel with key_mode decrypt")
	// ErrInvalidContentKeys: the request's keys are empty, malformed or repeat
	// a KID.
	ErrInvalidContentKeys = errors.New("invalid content keys")
	// ErrContentKeyExists: the channel already stores a key for one of the KIDs.
	// Keys are only ever added, never replaced.
	ErrContentKeyExists = errors.New("content key already exists")
)

// AddContentKeys appends "kid:key" pairs to a DASH decrypt channel and returns
// the KIDs added. It is all or nothing: a KID the channel already holds fails
// the whole request with ErrContentKeyExists and stores nothing. The running
// channel picks the keys up without a restart.
func (m *Manager) AddContentKeys(ctx context.Context, id string, keys []string) ([]string, error) {
	defer m.lockOperation(id)()

	cfg, ok := m.store.Get(id)
	if !ok {
		return nil, ErrChannelNotFound
	}
	if !cfg.IsDASHDecrypt() {
		return nil, ErrKeysNotApplicable
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: no keys given", ErrInvalidContentKeys)
	}
	parsed, err := parseContentKeys(keys)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidContentKeys, err)
	}

	m.mu.RLock()
	rt := m.runtimes[id]
	m.mu.RUnlock()
	if rt != nil {
		rt.configMu.Lock()
		defer rt.configMu.Unlock()
	}

	var dup string
	merged, err := m.store.Update(id, func(c *Config) {
		have := make(map[cenc.KID]bool, len(c.ContentKeys))
		for _, s := range c.ContentKeys {
			if k, err := cenc.ParseKey(s); err == nil {
				have[k.KID] = true
			}
		}
		for _, k := range parsed {
			if have[k.KID] {
				dup = k.KID.String()
				return
			}
		}
		for _, k := range parsed {
			c.ContentKeys = append(c.ContentKeys, k.Format())
		}
	})
	if err != nil {
		return nil, fmt.Errorf("store error: %w", err)
	}
	if dup != "" {
		return nil, fmt.Errorf("%w: KID %s", ErrContentKeyExists, dup)
	}

	added := make([]string, len(parsed))
	for i, k := range parsed {
		added[i] = k.KID.String()
	}
	if rt != nil {
		rt.setConfig(merged)
		rt.onKeysAdded(parsed)
	}
	return added, nil
}

// onKeysAdded tells the running channel that keys were added to its
// configuration. AdaptationSets decided from now on can use them, and the
// channel's conversion goroutine converts those already stored encrypted
// that the keys complete (runConversions).
//
// A representation whose init was refused, or whose segments were dropped
// because their samples rotated to a KID, for want of one of these keys is
// tried again (Registry.Retriable): the watcher offers its segments anew
// (Watcher.Rediscover), and the processor prepares its init again for any of
// them because keys were added since it failed (Registry.KeyEpoch). A live
// origin's next poll does so; a static one is enumerated once more while the
// channel still ingests. A channel that fetches nothing more, a stalled live
// one or a VOD, tries them again only when ingest runs again: a restart
// before the VOD transition, or a restore to live after it.
func (rt *Runtime) onKeysAdded(keys []cenc.Key) {
	if rt.decrypt == nil {
		return
	}
	added := rt.decrypt.AddKeys(keys)
	if rt.watcher == nil {
		return
	}
	var reps []string
	for _, r := range rt.decrypt.Retriable(added) {
		reps = append(reps, queue.RepKey(rt.ID, r.AS.PeriodID, r.AS.ASID, r.RepID))
	}
	rt.watcher.Rediscover(reps)
}
