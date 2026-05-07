package responsesstate

import (
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// Store keeps a short-lived mapping from a Responses API response id to the
// account that produced it and the full transcript accumulated so far. When a
// client later sends previous_response_id, the gateway can keep routing to the
// same account for cheap upstream context reuse, or reconstruct the transcript
// locally if it must fail over to a different account.
type Store struct {
	mu         sync.Mutex
	limit      int
	maxBytes   int64
	ttl        time.Duration
	totalBytes int64
	byID       map[string]*Session
	byThread   map[string]*ThreadAffinity
}

type Session struct {
	ResponseID string
	AccountID  string
	ThreadKey  string
	Transcript []json.RawMessage
	CreatedAt  time.Time
	LastSeen   time.Time
	Bytes      int64
}

type ThreadAffinity struct {
	ThreadKey  string
	AccountID  string
	ResponseID string
	Transcript []json.RawMessage
	CreatedAt  time.Time
	LastSeen   time.Time
	Bytes      int64
}

func New(limit int, ttl time.Duration) *Store {
	return NewWithByteLimit(limit, ttl, 0)
}

func NewWithByteLimit(limit int, ttl time.Duration, maxBytes int64) *Store {
	if limit <= 0 {
		limit = 10000
	}
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &Store{
		limit:    limit,
		maxBytes: maxBytes,
		ttl:      ttl,
		byID:     make(map[string]*Session),
		byThread: make(map[string]*ThreadAffinity),
	}
}

func (s *Store) Lookup(responseID string) (Session, bool) {
	if responseID == "" {
		return Session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.byID[responseID]
	if !ok {
		return Session{}, false
	}
	if time.Since(cur.LastSeen) > s.ttl {
		s.totalBytes -= cur.Bytes
		delete(s.byID, responseID)
		return Session{}, false
	}
	cur.LastSeen = time.Now()
	out := cloneSession(cur)
	if len(out.Transcript) == 0 && cur.ThreadKey != "" {
		if thread, ok := s.byThread[cur.ThreadKey]; ok && thread.ResponseID == responseID && len(thread.Transcript) > 0 {
			if time.Since(thread.LastSeen) > s.ttl {
				s.totalBytes -= thread.Bytes
				delete(s.byThread, cur.ThreadKey)
			} else {
				thread.LastSeen = time.Now()
				out.Transcript = cloneTranscript(thread.Transcript)
				out.Bytes = thread.Bytes
			}
		}
	}
	return out, true
}

func (s *Store) LookupThread(threadKey string) (ThreadAffinity, bool) {
	if threadKey == "" {
		return ThreadAffinity{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.byThread[threadKey]
	if !ok {
		return ThreadAffinity{}, false
	}
	if time.Since(cur.LastSeen) > s.ttl {
		s.totalBytes -= cur.Bytes
		delete(s.byThread, threadKey)
		return ThreadAffinity{}, false
	}
	cur.LastSeen = time.Now()
	return ThreadAffinity{
		ThreadKey:  cur.ThreadKey,
		AccountID:  cur.AccountID,
		ResponseID: cur.ResponseID,
		CreatedAt:  cur.CreatedAt,
		LastSeen:   cur.LastSeen,
	}, true
}

func (s *Store) LookupThreadTranscript(threadKey string) (Session, bool) {
	if threadKey == "" {
		return Session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.byThread[threadKey]
	if !ok {
		return Session{}, false
	}
	if time.Since(cur.LastSeen) > s.ttl {
		s.totalBytes -= cur.Bytes
		delete(s.byThread, threadKey)
		return Session{}, false
	}
	cur.LastSeen = time.Now()
	if len(cur.Transcript) == 0 || cur.ResponseID == "" {
		return Session{}, false
	}
	return Session{
		ResponseID: cur.ResponseID,
		AccountID:  cur.AccountID,
		ThreadKey:  cur.ThreadKey,
		Transcript: cloneTranscript(cur.Transcript),
		CreatedAt:  cur.CreatedAt,
		LastSeen:   cur.LastSeen,
		Bytes:      cur.Bytes,
	}, true
}

func (s *Store) Record(responseID, accountID string, transcript []json.RawMessage) {
	if responseID == "" || accountID == "" || len(transcript) == 0 {
		return
	}
	now := time.Now()
	cloned := cloneTranscript(transcript)
	bytes := transcriptBytes(cloned)
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.byID[responseID]; ok {
		s.totalBytes -= old.Bytes
	}
	s.byID[responseID] = &Session{
		ResponseID: responseID,
		AccountID:  accountID,
		Transcript: cloned,
		CreatedAt:  now,
		LastSeen:   now,
		Bytes:      bytes,
	}
	s.totalBytes += bytes
	s.evict(responseID)
}

func (s *Store) RecordWithThread(responseID, accountID, threadKey string, transcript []json.RawMessage) {
	if threadKey == "" {
		s.Record(responseID, accountID, transcript)
		return
	}
	if responseID == "" || accountID == "" || len(transcript) == 0 {
		return
	}
	now := time.Now()
	cloned := cloneTranscript(transcript)
	bytes := transcriptBytes(cloned)
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.byID[responseID]; ok {
		s.totalBytes -= old.Bytes
	}
	s.byID[responseID] = &Session{
		ResponseID: responseID,
		AccountID:  accountID,
		ThreadKey:  threadKey,
		CreatedAt:  now,
		LastSeen:   now,
	}
	if old, ok := s.byThread[threadKey]; ok {
		s.totalBytes -= old.Bytes
	}
	s.byThread[threadKey] = &ThreadAffinity{
		ThreadKey:  threadKey,
		AccountID:  accountID,
		ResponseID: responseID,
		Transcript: cloned,
		CreatedAt:  now,
		LastSeen:   now,
		Bytes:      bytes,
	}
	s.totalBytes += bytes
	s.evict(responseID)
	s.evictThreads(threadKey)
}

// RecordThread keeps a stable Responses thread on the same upstream account
// even if an older response id mapping has expired or its transcript was
// released. The key should already be scoped by caller tenant/group/provider.
func (s *Store) RecordThread(threadKey, accountID string) {
	if threadKey == "" || accountID == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.byThread[threadKey]; ok {
		s.totalBytes -= old.Bytes
		old.AccountID = accountID
		old.ResponseID = ""
		old.Transcript = nil
		old.Bytes = 0
		old.LastSeen = now
		return
	}
	s.byThread[threadKey] = &ThreadAffinity{
		ThreadKey: threadKey,
		AccountID: accountID,
		CreatedAt: now,
		LastSeen:  now,
	}
	s.evictThreads(threadKey)
}

// RecordAccount keeps lightweight affinity when a transcript is unavailable or
// has been released under byte pressure. This preserves previous_response_id
// routing to the same upstream account without retaining another long context.
func (s *Store) RecordAccount(responseID, accountID string) {
	if responseID == "" || accountID == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.byID[responseID]; ok {
		s.totalBytes -= old.Bytes
		if old.ThreadKey != "" {
			if thread, ok := s.byThread[old.ThreadKey]; ok && thread.ResponseID == responseID {
				s.totalBytes -= thread.Bytes
				thread.ResponseID = ""
				thread.Transcript = nil
				thread.Bytes = 0
			}
		}
	}
	s.byID[responseID] = &Session{
		ResponseID: responseID,
		AccountID:  accountID,
		CreatedAt:  now,
		LastSeen:   now,
	}
	s.evict(responseID)
}

func (s *Store) evict(keepID string) {
	type item struct {
		id       string
		lastSeen time.Time
	}
	if len(s.byID) <= s.limit && (s.maxBytes <= 0 || s.totalBytes <= s.maxBytes) {
		return
	}
	items := make([]item, 0, len(s.byID))
	for id, sess := range s.byID {
		items = append(items, item{id: id, lastSeen: sess.LastSeen})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].lastSeen.Before(items[j].lastSeen)
	})

	// Byte pressure drops old transcript payloads first, but keeps the account
	// mapping. That avoids routing a future previous_response_id to a different
	// account and corrupting long-context continuity.
	if s.maxBytes > 0 && s.totalBytes > s.maxBytes {
		for _, item := range items {
			if s.totalBytes <= s.maxBytes {
				break
			}
			if item.id == keepID {
				continue
			}
			if old, ok := s.byID[item.id]; ok && old.Bytes > 0 {
				s.totalBytes -= old.Bytes
				old.Transcript = nil
				old.Bytes = 0
			}
		}
	}
	if s.maxBytes > 0 && s.totalBytes > s.maxBytes {
		type threadItem struct {
			key      string
			lastSeen time.Time
		}
		threadItems := make([]threadItem, 0, len(s.byThread))
		for key, thread := range s.byThread {
			threadItems = append(threadItems, threadItem{key: key, lastSeen: thread.LastSeen})
		}
		sort.Slice(threadItems, func(i, j int) bool {
			return threadItems[i].lastSeen.Before(threadItems[j].lastSeen)
		})
		for _, item := range threadItems {
			if s.totalBytes <= s.maxBytes {
				break
			}
			if thread, ok := s.byThread[item.key]; ok && thread.ResponseID != keepID && thread.Bytes > 0 {
				s.totalBytes -= thread.Bytes
				thread.ResponseID = ""
				thread.Transcript = nil
				thread.Bytes = 0
			}
		}
	}

	if len(s.byID) <= s.limit {
		return
	}
	for _, item := range items {
		if item.id == keepID && len(s.byID) == 1 {
			return
		}
		if item.id == keepID {
			continue
		}
		if len(s.byID) <= s.limit {
			return
		}
		if old, ok := s.byID[item.id]; ok {
			s.totalBytes -= old.Bytes
			delete(s.byID, item.id)
		}
	}
}

func (s *Store) evictThreads(keepKey string) {
	if len(s.byThread) <= s.limit {
		return
	}
	type item struct {
		key      string
		lastSeen time.Time
	}
	items := make([]item, 0, len(s.byThread))
	for key, thread := range s.byThread {
		items = append(items, item{key: key, lastSeen: thread.LastSeen})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].lastSeen.Before(items[j].lastSeen)
	})
	for _, item := range items {
		if item.key == keepKey && len(s.byThread) == 1 {
			return
		}
		if item.key == keepKey {
			continue
		}
		if len(s.byThread) <= s.limit {
			return
		}
		delete(s.byThread, item.key)
	}
}

func cloneSession(in *Session) Session {
	if in == nil {
		return Session{}
	}
	return Session{
		ResponseID: in.ResponseID,
		AccountID:  in.AccountID,
		ThreadKey:  in.ThreadKey,
		Transcript: cloneTranscript(in.Transcript),
		CreatedAt:  in.CreatedAt,
		LastSeen:   in.LastSeen,
		Bytes:      in.Bytes,
	}
}

func cloneTranscript(in []json.RawMessage) []json.RawMessage {
	if len(in) == 0 {
		return nil
	}
	out := make([]json.RawMessage, 0, len(in))
	for _, item := range in {
		cp := make([]byte, len(item))
		copy(cp, item)
		out = append(out, json.RawMessage(cp))
	}
	return out
}

func transcriptBytes(in []json.RawMessage) int64 {
	var n int64
	for _, item := range in {
		n += int64(len(item))
	}
	return n
}
