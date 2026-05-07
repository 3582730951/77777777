// Package enrollment handles the "OAuth-like" account onboarding flow:
//
//   1. Admin clicks "Add Account" → Server creates a pending enrollment with
//      a unique enroll_id and 10-minute TTL.
//   2. UI shows a one-line bookmarklet that, when run from a logged-in
//      chatgpt.com tab, captures cookies + the /api/auth/session JSON and
//      POSTs to our public /enroll/{id} endpoint.
//   3. Server receives, parses, saves the credential to the encrypted store
//      and registers the account with the scheduler.
//   4. Admin's UI polls /enroll/{id}/status and auto-redirects to the new
//      account detail page when complete.
//
// CORS: the enrollment POST endpoint is *open* (Access-Control-Allow-Origin:
// *) because the bookmarklet runs in chatgpt.com origin, but it's gated by
// the enroll_id that only an authenticated admin could have generated.
package enrollment

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

type State string

const (
	StatePending   State = "pending"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateExpired   State = "expired"
)

type Pending struct {
	ID         string
	TenantID   string
	Provider   string
	GroupID    string
	Note       string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	State      State
	AccountID  string // populated on success
	Error      string // populated on failure
	CompletedAt time.Time
}

type Manager struct {
	mu      sync.Mutex
	pending map[string]*Pending
	ttl     time.Duration
}

func New() *Manager {
	return &Manager{
		pending: map[string]*Pending{},
		ttl:     10 * time.Minute,
	}
}

func (m *Manager) Create(tenantID, provider, groupID, note string) *Pending {
	id := randHex(16)
	p := &Pending{
		ID:        id,
		TenantID:  tenantID,
		Provider:  provider,
		GroupID:   groupID,
		Note:      note,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(m.ttl),
		State:     StatePending,
	}
	m.mu.Lock()
	m.pending[id] = p
	m.gcLocked()
	m.mu.Unlock()
	return p
}

func (m *Manager) Get(id string) (*Pending, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pending[id]
	if !ok {
		return nil, false
	}
	if time.Now().After(p.ExpiresAt) && p.State == StatePending {
		p.State = StateExpired
	}
	cp := *p
	return &cp, true
}

func (m *Manager) Complete(id, accountID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.pending[id]; ok {
		p.State = StateCompleted
		p.AccountID = accountID
		p.CompletedAt = time.Now()
	}
}

func (m *Manager) Fail(id, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.pending[id]; ok {
		p.State = StateFailed
		p.Error = errMsg
		p.CompletedAt = time.Now()
	}
}

func (m *Manager) gcLocked() {
	now := time.Now()
	for id, p := range m.pending {
		if now.Sub(p.CompletedAt) > 1*time.Hour && p.State != StatePending {
			delete(m.pending, id)
		}
		if now.After(p.ExpiresAt) && p.State == StatePending {
			p.State = StateExpired
		}
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
