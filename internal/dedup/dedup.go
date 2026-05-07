// Package dedup keeps a process-local SHA256 → upstream-file-id map so the same
// image bytes uploaded multiple times within an agent loop reuse the same
// upstream reference instead of re-uploading. This is lossless: the model
// still sees the full image content, we just save bandwidth and provider-side
// storage cost.
package dedup

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

type Cache struct {
	mu    sync.RWMutex
	limit int
	ttl   time.Duration
	byKey map[string]entry
}

type entry struct {
	value     string
	createdAt time.Time
	hits      int64
}

func New(limit int, ttl time.Duration) *Cache {
	if limit <= 0 {
		limit = 5000
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Cache{limit: limit, ttl: ttl, byKey: map[string]entry{}}
}

// Hash computes the deterministic key for content.
func Hash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Lookup returns (upstream id, hit) for content already cached.
func (c *Cache) Lookup(b []byte) (string, bool) {
	if len(b) == 0 {
		return "", false
	}
	k := Hash(b)
	c.mu.RLock()
	e, ok := c.byKey[k]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	if time.Since(e.createdAt) > c.ttl {
		c.mu.Lock()
		delete(c.byKey, k)
		c.mu.Unlock()
		return "", false
	}
	c.mu.Lock()
	e.hits++
	c.byKey[k] = e
	c.mu.Unlock()
	return e.value, true
}

// Set records a fresh upstream id for content.
func (c *Cache) Set(b []byte, upstreamID string) {
	if len(b) == 0 || upstreamID == "" {
		return
	}
	k := Hash(b)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byKey[k] = entry{value: upstreamID, createdAt: time.Now()}
	if len(c.byKey) > c.limit {
		// drop oldest 10%
		var oldest []string
		oldestAt := time.Now()
		for kk, ee := range c.byKey {
			if ee.createdAt.Before(oldestAt) {
				oldestAt = ee.createdAt
				oldest = append([]string{kk}, oldest...)
			}
		}
		drop := len(c.byKey) - c.limit
		for i := 0; i < drop && i < len(oldest); i++ {
			delete(c.byKey, oldest[i])
		}
	}
}

func (c *Cache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.byKey)
}
