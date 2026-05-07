// Package audit centralises audit log emission. Events are persisted to SQLite
// (via store.AppendAudit) and fan-out broadcast to SSE subscribers so the
// admin UI's live log panel updates in real time.
package audit

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/llm-pool/gateway/internal/store"
)

type Event struct {
	ID        int64     `json:"id"`
	At        time.Time `json:"at"`
	Level     string    `json:"level"`
	Category  string    `json:"category"`
	AccountID string    `json:"account_id,omitempty"`
	GroupID   string    `json:"group_id,omitempty"`
	Message   string    `json:"message"`
}

// Logger is the singleton used by all subsystems to record audit events.
type Logger struct {
	store      *store.Store
	mu         sync.RWMutex
	subs       map[chan Event]struct{}
	nextID     int64
}

func NewLogger(s *store.Store) *Logger {
	return &Logger{store: s, subs: make(map[chan Event]struct{})}
}

func (l *Logger) Log(level, category, accountID, groupID, msg string) {
	if l == nil {
		return
	}
	if l.store != nil {
		l.store.AppendAudit(context.Background(), level, category, accountID, groupID, msg)
	}
	ev := Event{
		ID:        atomic.AddInt64(&l.nextID, 1),
		At:        time.Now(),
		Level:     level,
		Category:  category,
		AccountID: accountID,
		GroupID:   groupID,
		Message:   msg,
	}
	l.broadcast(ev)
}

func (l *Logger) broadcast(ev Event) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for ch := range l.subs {
		select {
		case ch <- ev:
		default:
			// subscriber too slow — drop event for it
		}
	}
}

// Subscribe registers a buffered receiver. The returned cancel func deregisters.
func (l *Logger) Subscribe(buf int) (<-chan Event, func()) {
	if buf <= 0 {
		buf = 32
	}
	ch := make(chan Event, buf)
	l.mu.Lock()
	l.subs[ch] = struct{}{}
	l.mu.Unlock()
	return ch, func() {
		l.mu.Lock()
		delete(l.subs, ch)
		close(ch)
		l.mu.Unlock()
	}
}
