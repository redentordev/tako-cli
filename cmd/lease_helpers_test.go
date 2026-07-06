package cmd

import (
	"sync"

	remotestate "github.com/redentordev/tako-cli/internal/state"
)

type recordingLeaseManager struct {
	mu       sync.Mutex
	released []string
}

func (m *recordingLeaseManager) ReleaseLease(lease *remotestate.LeaseInfo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.released = append(m.released, lease.ID)
	return nil
}

func (m *recordingLeaseManager) Released() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.released...)
}
