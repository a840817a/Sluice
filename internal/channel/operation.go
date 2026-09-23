package channel

import "sync"

type channelOperationLock struct {
	mu    sync.Mutex
	users int
}

// lockOperation acquires the shared shutdown gate and the lifecycle lock for
// id, and returns the function that releases both.
func (m *Manager) lockOperation(id string) (unlock func()) {
	m.operationGate.RLock()
	m.operationLocksMu.Lock()
	op := m.operationLocks[id]
	if op == nil {
		op = &channelOperationLock{}
		m.operationLocks[id] = op
	}
	op.users++
	m.operationLocksMu.Unlock()
	op.mu.Lock()
	return func() {
		op.mu.Unlock()
		m.operationLocksMu.Lock()
		op.users--
		if op.users == 0 {
			delete(m.operationLocks, id)
		}
		m.operationLocksMu.Unlock()
		m.operationGate.RUnlock()
	}
}
