package conversation

import (
	"sync"
	"testing"
	"time"
)

func TestNew_Defaults(t *testing.T) {
	m := New(0, 0)
	if m.limit != 10000 {
		t.Errorf("expected default limit 10000, got %d", m.limit)
	}
	if m.ttl != 30*time.Minute {
		t.Errorf("expected default ttl 30m, got %v", m.ttl)
	}
}

func TestLookup_EmptyHash(t *testing.T) {
	m := New(100, time.Minute)
	_, ok := m.Lookup("")
	if ok {
		t.Error("empty hash should return false")
	}
}

func TestLookup_Miss(t *testing.T) {
	m := New(100, time.Minute)
	_, ok := m.Lookup("nonexistent")
	if ok {
		t.Error("missing key should return false")
	}
}

func TestRecord_NewEntry(t *testing.T) {
	m := New(100, time.Minute)
	hit := m.Record("h1", "acc1", "key1", "t1", "g1", 0)
	if hit {
		t.Error("new entry should be a miss")
	}
	e, ok := m.Lookup("h1")
	if !ok {
		t.Fatal("should find recorded entry")
	}
	if e.AccountID != "acc1" || e.APIKey != "key1" || e.TenantID != "t1" || e.GroupID != "g1" {
		t.Error("entry fields mismatch")
	}
	if e.MissCount != 1 {
		t.Errorf("expected MissCount=1, got %d", e.MissCount)
	}
}

func TestRecord_Hit(t *testing.T) {
	m := New(100, time.Minute)
	m.Record("h1", "acc1", "key1", "t1", "g1", 0)
	hit := m.Record("h1", "acc1", "key1", "t1", "g1", 1)
	if !hit {
		t.Error("same account should be a hit")
	}
	e, _ := m.Lookup("h1")
	if e.HitCount != 1 {
		t.Errorf("expected HitCount=1, got %d", e.HitCount)
	}
	if e.LastMsgIndex != 1 {
		t.Errorf("expected LastMsgIndex=1, got %d", e.LastMsgIndex)
	}
}

func TestRecord_DifferentAccount(t *testing.T) {
	m := New(100, time.Minute)
	m.Record("h1", "acc1", "key1", "t1", "g1", 0)
	hit := m.Record("h1", "acc2", "key1", "t1", "g1", 1)
	if hit {
		t.Error("different account should be a miss")
	}
	e, _ := m.Lookup("h1")
	if e.AccountID != "acc2" {
		t.Error("account should be updated")
	}
	if e.MissCount != 2 {
		t.Errorf("expected MissCount=2, got %d", e.MissCount)
	}
}

func TestRecord_EmptyHash(t *testing.T) {
	m := New(100, time.Minute)
	hit := m.Record("", "acc1", "key1", "t1", "g1", 0)
	if hit {
		t.Error("empty hash should return false")
	}
}

func TestLookup_TTLExpiry(t *testing.T) {
	m := New(100, 1*time.Millisecond)
	m.Record("h1", "acc1", "key1", "t1", "g1", 0)
	time.Sleep(5 * time.Millisecond)
	_, ok := m.Lookup("h1")
	if ok {
		t.Error("expired entry should not be found")
	}
}

func TestRecord_TTLExpiry_Recreates(t *testing.T) {
	m := New(100, 1*time.Millisecond)
	m.Record("h1", "acc1", "key1", "t1", "g1", 0)
	time.Sleep(5 * time.Millisecond)
	hit := m.Record("h1", "acc2", "key2", "t2", "g2", 0)
	if hit {
		t.Error("expired entry re-record should be a miss")
	}
	e, ok := m.Lookup("h1")
	if !ok {
		t.Fatal("re-recorded entry should be found")
	}
	if e.AccountID != "acc2" {
		t.Error("should have new account")
	}
}

func TestEviction(t *testing.T) {
	m := New(5, time.Minute)
	for i := 0; i < 10; i++ {
		m.Record(string(rune('a'+i)), "acc", "k", "t", "g", 0)
	}
	s := m.Stats()
	if s.Entries > 5 {
		t.Errorf("expected <=5 entries after eviction, got %d", s.Entries)
	}
}

func TestSetUpstreamConv(t *testing.T) {
	m := New(100, time.Minute)
	m.Record("h1", "acc1", "key1", "t1", "g1", 0)
	m.SetUpstreamConv("h1", "conv-123")
	e, _ := m.Lookup("h1")
	if e.UpstreamConvID != "conv-123" {
		t.Errorf("expected conv-123, got %s", e.UpstreamConvID)
	}
}

func TestSetUpstreamConv_Missing(t *testing.T) {
	m := New(100, time.Minute)
	m.SetUpstreamConv("missing", "conv") // should not panic
}

func TestStats(t *testing.T) {
	m := New(100, time.Minute)
	m.Record("h1", "acc1", "key1", "t1", "g1", 0)
	m.Record("h1", "acc1", "key1", "t1", "g1", 1) // hit
	m.Record("h2", "acc2", "key2", "t1", "g1", 0)
	s := m.Stats()
	if s.Entries != 2 {
		t.Errorf("expected 2 entries, got %d", s.Entries)
	}
	if s.Hits != 1 {
		t.Errorf("expected 1 hit, got %d", s.Hits)
	}
	if s.Misses != 2 {
		t.Errorf("expected 2 misses, got %d", s.Misses)
	}
}

func TestStatsByAPIKey(t *testing.T) {
	m := New(100, time.Minute)
	m.Record("h1", "acc1", "key1", "t1", "g1", 0)
	m.Record("h1", "acc1", "key1", "t1", "g1", 1)
	m.Record("h2", "acc2", "key2", "t1", "g1", 0)
	stats := m.StatsByAPIKey()
	if len(stats) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(stats))
	}
}

func TestConcurrency(t *testing.T) {
	m := New(1000, time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a' + i%26))
			m.Record(key, "acc", "k", "t", "g", i)
			m.Lookup(key)
			m.Stats()
		}(i)
	}
	wg.Wait()
}
