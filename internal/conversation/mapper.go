// Package conversation tracks conversation prefixes across requests so the
// gateway can (a) bind subsequent turns to the same upstream account (sticky
// routing) and (b) record whether the upstream provider would see a prefix
// it has already cached. The hit/miss signal feeds the cache-hit dashboard.
package conversation

import (
	"sort"
	"sync"
	"time"
)

// Mapper is an in-memory LRU keyed by the conversation prefix hash. Each
// entry remembers the upstream account that last served the same prefix and
// the size of the prefix it acknowledged. When a follow-up turn arrives we
// can shortcut the upstream call to "send only the new turn" and the request
// counts as a cache hit.
type Mapper struct {
	mu     sync.Mutex
	limit  int
	ttl    time.Duration
	byHash map[string]*Entry
}

type Entry struct {
	ConvHash         string
	AccountID        string
	UpstreamConvID   string
	APIKey           string
	TenantID         string
	GroupID          string
	LastMsgIndex     int
	HitCount         int64
	MissCount        int64
	LastSeen         time.Time
	CreatedAt        time.Time
}

func New(limit int, ttl time.Duration) *Mapper {
	if limit <= 0 {
		limit = 10000
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &Mapper{limit: limit, ttl: ttl, byHash: map[string]*Entry{}}
}

// Lookup returns the entry for a given hash, freshly bumping its LastSeen.
// Returns ok=false if missing or expired.
func (m *Mapper) Lookup(hash string) (Entry, bool) {
	if hash == "" {
		return Entry{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.byHash[hash]
	if !ok {
		return Entry{}, false
	}
	if time.Since(e.LastSeen) > m.ttl {
		delete(m.byHash, hash)
		return Entry{}, false
	}
	e.LastSeen = time.Now()
	return *e, true
}

// Record creates or updates an entry. Returns whether this update is a hit
// (existing prefix was extended on the same account) or a miss (new conv).
func (m *Mapper) Record(hash, accountID, apiKey, tenantID, groupID string, msgIdx int) (hit bool) {
	if hash == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	e, ok := m.byHash[hash]
	if ok && time.Since(e.LastSeen) <= m.ttl {
		hit = e.AccountID == accountID
		e.LastSeen = now
		e.AccountID = accountID
		if msgIdx > e.LastMsgIndex {
			e.LastMsgIndex = msgIdx
		}
		if hit {
			e.HitCount++
		} else {
			e.MissCount++
		}
		return hit
	}
	// Brand new entry — that counts as a miss for cache purposes.
	e = &Entry{
		ConvHash:     hash,
		AccountID:    accountID,
		APIKey:       apiKey,
		TenantID:     tenantID,
		GroupID:      groupID,
		LastMsgIndex: msgIdx,
		MissCount:    1,
		CreatedAt:    now,
		LastSeen:     now,
	}
	m.byHash[hash] = e
	if len(m.byHash) > m.limit {
		m.evict()
	}
	return false
}

// SetUpstreamConv records the upstream-side conversation id once the
// provider tells us about it. Subsequent calls can then issue an incremental
// turn instead of re-sending the full history.
func (m *Mapper) SetUpstreamConv(hash, upstreamConvID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.byHash[hash]; ok {
		e.UpstreamConvID = upstreamConvID
	}
}

func (m *Mapper) evict() {
	type kv struct {
		k string
		t time.Time
	}
	all := make([]kv, 0, len(m.byHash))
	for k, e := range m.byHash {
		all = append(all, kv{k, e.LastSeen})
	}
	if len(all) <= m.limit {
		return
	}
	target := len(all) - m.limit + len(all)/10
	if target < 1 {
		target = 1
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
	for i := 0; i < target; i++ {
		delete(m.byHash, all[i].k)
	}
}

// Stats returns aggregated hit/miss totals for the dashboard.
type Stats struct {
	Entries int
	Hits    int64
	Misses  int64
}

func (m *Mapper) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s Stats
	s.Entries = len(m.byHash)
	for _, e := range m.byHash {
		s.Hits += e.HitCount
		s.Misses += e.MissCount
	}
	return s
}

// StatsByAPIKey returns per-apikey hit/miss tallies.
type KeyStats struct {
	APIKey string
	Hits   int64
	Misses int64
}

func (m *Mapper) StatsByAPIKey() []KeyStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	by := map[string]*KeyStats{}
	for _, e := range m.byHash {
		if e.APIKey == "" {
			continue
		}
		k, ok := by[e.APIKey]
		if !ok {
			k = &KeyStats{APIKey: e.APIKey}
			by[e.APIKey] = k
		}
		k.Hits += e.HitCount
		k.Misses += e.MissCount
	}
	out := make([]KeyStats, 0, len(by))
	for _, v := range by {
		out = append(out, *v)
	}
	return out
}
