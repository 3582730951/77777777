package gemini

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// prefixCache maps a hash of the system+tools prefix to a Gemini
// CachedContent resource name. When a request matches an existing
// cached prefix, we reference it instead of resending the full prefix,
// saving ~75% on input token costs.
type prefixCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	name      string // cachedContents/{id}
	expiresAt time.Time
}

func newPrefixCache() *prefixCache {
	return &prefixCache{entries: make(map[string]cacheEntry)}
}

func (c *prefixCache) lookup(hash string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[hash]
	if !ok || time.Now().After(e.expiresAt) {
		if ok {
			delete(c.entries, hash)
		}
		return "", false
	}
	return e.name, true
}

func (c *prefixCache) store(hash, name string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[hash] = cacheEntry{name: name, expiresAt: time.Now().Add(ttl)}
	// Evict expired entries periodically (cheap: only on store).
	if len(c.entries) > 100 {
		now := time.Now()
		for k, v := range c.entries {
			if now.After(v.expiresAt) {
				delete(c.entries, k)
			}
		}
	}
}

func hashPrefix(system string, tools []byte) string {
	h := sha256.New()
	h.Write([]byte(system))
	h.Write([]byte{0})
	h.Write(tools)
	return hex.EncodeToString(h.Sum(nil))[:16]
}
