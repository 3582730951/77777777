// Package cache provides an optional Redis-backed caching layer for the gateway.
// When Redis is unavailable, operations gracefully degrade to no-op (fail-open).
// Used for: API key lookup cache, conversation state persistence, RPM tracking.
package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Client wraps go-redis with gateway-specific caching operations.
// All methods are safe to call when the underlying client is nil (disabled mode).
type Client struct {
	rdb *redis.Client
}

// New creates a Redis cache client. Returns a no-op client if addr is empty.
func New(addr, password string, db int) *Client {
	if addr == "" {
		return &Client{}
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		PoolSize:     10,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		slog.Warn("redis unavailable, running without cache", "addr", addr, "err", err)
		return &Client{}
	}
	slog.Info("redis connected", "addr", addr)
	return &Client{rdb: rdb}
}

func (c *Client) Enabled() bool { return c.rdb != nil }

func (c *Client) Close() error {
	if c.rdb != nil {
		return c.rdb.Close()
	}
	return nil
}

// --- API Key Cache ---

const keyPrefix = "pool:apikey:"

type CachedKey struct {
	TenantID string `json:"t"`
	GroupID  string `json:"g"`
}

func (c *Client) GetAPIKey(ctx context.Context, key string) (CachedKey, bool) {
	if c.rdb == nil {
		return CachedKey{}, false
	}
	val, err := c.rdb.Get(ctx, keyPrefix+key).Bytes()
	if err != nil {
		return CachedKey{}, false
	}
	var ck CachedKey
	if json.Unmarshal(val, &ck) != nil {
		return CachedKey{}, false
	}
	return ck, true
}

func (c *Client) SetAPIKey(ctx context.Context, key string, ck CachedKey, ttl time.Duration) {
	if c.rdb == nil {
		return
	}
	b, _ := json.Marshal(ck)
	c.rdb.Set(ctx, keyPrefix+key, b, ttl)
}

func (c *Client) InvalidateAPIKey(ctx context.Context, key string) {
	if c.rdb == nil {
		return
	}
	c.rdb.Del(ctx, keyPrefix+key)
}

// --- Conversation State Persistence ---

const convPrefix = "pool:conv:"

func (c *Client) GetConv(ctx context.Context, hash string) (string, bool) {
	if c.rdb == nil {
		return "", false
	}
	val, err := c.rdb.Get(ctx, convPrefix+hash).Result()
	if err != nil {
		return "", false
	}
	return val, true
}

func (c *Client) SetConv(ctx context.Context, hash, accountID string, ttl time.Duration) {
	if c.rdb == nil {
		return
	}
	c.rdb.Set(ctx, convPrefix+hash, accountID, ttl)
}

// --- Per-Key RPM Tracking (Sliding Window) ---

const rpmPrefix = "pool:rpm:"

// IncrRPM increments the request count for a key in the current minute window.
// Returns the current count after increment.
func (c *Client) IncrRPM(ctx context.Context, key string) int64 {
	if c.rdb == nil {
		return 0
	}
	rkey := fmt.Sprintf("%s%s:%d", rpmPrefix, key, time.Now().Unix()/60)
	pipe := c.rdb.Pipeline()
	incr := pipe.Incr(ctx, rkey)
	pipe.Expire(ctx, rkey, 2*time.Minute)
	pipe.Exec(ctx)
	return incr.Val()
}

// GetRPM returns the current RPM for a key.
func (c *Client) GetRPM(ctx context.Context, key string) int64 {
	if c.rdb == nil {
		return 0
	}
	rkey := fmt.Sprintf("%s%s:%d", rpmPrefix, key, time.Now().Unix()/60)
	val, _ := c.rdb.Get(ctx, rkey).Int64()
	return val
}

// --- Generic Cache ---

func (c *Client) Set(ctx context.Context, key string, value any, ttl time.Duration) {
	if c.rdb == nil {
		return
	}
	b, err := json.Marshal(value)
	if err != nil {
		return
	}
	c.rdb.Set(ctx, key, b, ttl)
}

func (c *Client) Get(ctx context.Context, key string, dest any) bool {
	if c.rdb == nil {
		return false
	}
	val, err := c.rdb.Get(ctx, key).Bytes()
	if err != nil {
		return false
	}
	return json.Unmarshal(val, dest) == nil
}
