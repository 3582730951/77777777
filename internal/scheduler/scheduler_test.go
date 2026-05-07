package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/domain"
)

func makeAcc(id, provider, tenantID string) *domain.Account {
	return &domain.Account{
		ID:       id,
		Provider: provider,
		TenantID: tenantID,
		State:    domain.StateActive,
	}
}

func defaultCfg() config.Scheduler {
	c := config.Scheduler{}
	c.EWMAAlpha = 0.3
	c.Breaker.FailThreshold = 3
	c.Breaker.OpenDuration = 5 * time.Second
	c.Sticky.Enabled = true
	c.Sticky.StickyTTL = 1 * time.Minute
	c.Failover.NeverFail.Enabled = true
	c.Failover.NeverFail.MaxWait = 3 * time.Second
	c.Failover.NeverFail.ProbeInterval = 100 * time.Millisecond
	return c
}

// TestPickHappyPath: two healthy accounts, P2C selects one.
func TestPickHappyPath(t *testing.T) {
	s := New(defaultCfg())
	s.Register(makeAcc("a1", "chatgpt", "t1"))
	s.Register(makeAcc("a2", "chatgpt", "t1"))
	slot, err := s.Pick(context.Background(), PickRequest{Provider: "chatgpt", TenantID: "t1"})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if slot.Account.ID != "a1" && slot.Account.ID != "a2" {
		t.Errorf("unexpected account: %s", slot.Account.ID)
	}
}

// TestStickyRouting: same convHash should return same account on repeated calls.
func TestStickyRouting(t *testing.T) {
	s := New(defaultCfg())
	s.Register(makeAcc("a1", "chatgpt", "t1"))
	s.Register(makeAcc("a2", "chatgpt", "t1"))
	first, err := s.Pick(context.Background(), PickRequest{Provider: "chatgpt", TenantID: "t1", ConvHash: "conv-X"})
	if err != nil {
		t.Fatalf("pick first: %v", err)
	}
	for i := 0; i < 5; i++ {
		second, err := s.Pick(context.Background(), PickRequest{Provider: "chatgpt", TenantID: "t1", ConvHash: "conv-X"})
		if err != nil {
			t.Fatalf("pick repeat: %v", err)
		}
		if first.Account.ID != second.Account.ID {
			t.Errorf("sticky broken: first=%s second=%s", first.Account.ID, second.Account.ID)
		}
	}
}

func TestPickExcludesFiveHourOnlyQuota(t *testing.T) {
	s := New(defaultCfg())
	a1 := makeAcc("a1", "chatgpt", "t1")
	a1.Quota.ShortWindow = domain.QuotaWindow{
		Limit:      100,
		Used:       10,
		ResetAt:    time.Now().Add(5 * time.Hour),
		Confidence: 1,
	}
	a2 := makeAcc("a2", "chatgpt", "t1")
	a2.Quota.ShortWindow = domain.QuotaWindow{
		Limit:      100,
		Used:       10,
		ResetAt:    time.Now().Add(5 * time.Hour),
		Confidence: 1,
	}
	a2.Quota.LongWindow = domain.QuotaWindow{
		Limit:      100,
		Used:       10,
		ResetAt:    time.Now().Add(7 * 24 * time.Hour),
		Confidence: 1,
	}
	s.Register(a1)
	s.Register(a2)

	slot, err := s.Pick(context.Background(), PickRequest{Provider: "chatgpt", TenantID: "t1"})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if slot.Account.ID != "a2" {
		t.Fatalf("expected account with both 5h and 7d quota, got %s", slot.Account.ID)
	}
}

func TestPickExcludesDepletedSevenDayQuota(t *testing.T) {
	cfg := defaultCfg()
	cfg.Failover.NeverFail.Enabled = false
	s := New(cfg)
	a := makeAcc("a1", "chatgpt", "t1")
	a.Quota.ShortWindow = domain.QuotaWindow{
		Limit:      100,
		Used:       1,
		ResetAt:    time.Now().Add(5 * time.Hour),
		Confidence: 1,
	}
	a.Quota.LongWindow = domain.QuotaWindow{
		Limit:      100,
		Used:       100,
		ResetAt:    time.Now().Add(7 * 24 * time.Hour),
		Confidence: 1,
	}
	s.Register(a)

	_, err := s.Pick(context.Background(), PickRequest{Provider: "chatgpt", TenantID: "t1"})
	if err == nil {
		t.Fatal("expected exhausted 7d quota to be excluded")
	}
}

func TestPickExcludesDepletedFiveHourQuota(t *testing.T) {
	cfg := defaultCfg()
	cfg.Failover.NeverFail.Enabled = false
	s := New(cfg)
	a := makeAcc("a1", "chatgpt", "t1")
	a.Quota.ShortWindow = domain.QuotaWindow{
		Limit:      100,
		Used:       100,
		ResetAt:    time.Now().Add(5 * time.Hour),
		Confidence: 1,
	}
	a.Quota.LongWindow = domain.QuotaWindow{
		Limit:      100,
		Used:       1,
		ResetAt:    time.Now().Add(7 * 24 * time.Hour),
		Confidence: 1,
	}
	s.Register(a)

	_, err := s.Pick(context.Background(), PickRequest{Provider: "chatgpt", TenantID: "t1"})
	if err == nil {
		t.Fatal("expected exhausted 5h quota to be excluded")
	}
}

func TestPickExcludesDepletedSevenDayQuotaWhenFiveHourUnknown(t *testing.T) {
	cfg := defaultCfg()
	cfg.Failover.NeverFail.Enabled = false
	s := New(cfg)
	a := makeAcc("a1", "gemini", "t1")
	a.Quota.LongWindow = domain.QuotaWindow{
		Limit:      100,
		Used:       100,
		ResetAt:    time.Now().Add(7 * 24 * time.Hour),
		Confidence: 1,
	}
	s.Register(a)

	_, err := s.Pick(context.Background(), PickRequest{Provider: "gemini", TenantID: "t1"})
	if err == nil {
		t.Fatal("expected exhausted 7d quota to be excluded even when 5h quota is unknown")
	}
}

func TestSnapshotHealthyExcludesZeroQuota(t *testing.T) {
	s := New(defaultCfg())
	a := makeAcc("a1", "chatgpt", "t1")
	a.Quota.ShortWindow = domain.QuotaWindow{
		Limit:      100,
		Used:       100,
		ResetAt:    time.Now().Add(5 * time.Hour),
		Confidence: 1,
	}
	a.Quota.LongWindow = domain.QuotaWindow{
		Limit:      100,
		Used:       1,
		ResetAt:    time.Now().Add(7 * 24 * time.Hour),
		Confidence: 1,
	}
	s.Register(a)

	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected one slot, got %d", len(snap))
	}
	if snap[0].Healthy {
		t.Fatal("account with exhausted 5h quota must not be counted healthy")
	}
}

func TestUpdateQuotaMarksZeroQuotaProbablyExhausted(t *testing.T) {
	s := New(defaultCfg())
	s.Register(makeAcc("a1", "chatgpt", "t1"))

	s.UpdateQuota("a1", &domain.QuotaState{
		ShortWindow: domain.QuotaWindow{
			Limit:      100,
			Used:       100,
			ResetAt:    time.Now().Add(5 * time.Hour),
			Confidence: 1,
		},
		LongWindow: domain.QuotaWindow{
			Limit:      100,
			Used:       1,
			ResetAt:    time.Now().Add(7 * 24 * time.Hour),
			Confidence: 1,
		},
	})

	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected one slot, got %d", len(snap))
	}
	if snap[0].Confidence != domain.ConfProbablyExhausted.String() {
		t.Fatalf("expected probably exhausted confidence, got %s", snap[0].Confidence)
	}
	if snap[0].Healthy {
		t.Fatal("zero-quota account should not be healthy after quota update")
	}
}

func TestPickAvoidsDrainingAccountForNewConversation(t *testing.T) {
	cfg := defaultCfg()
	cfg.Quota.DrainThreshold = 0.10
	s := New(cfg)
	draining := makeAcc("draining", "chatgpt", "t1")
	draining.Quota.ShortWindow = domain.QuotaWindow{Limit: 100, Used: 95, Confidence: 1}
	draining.Quota.LongWindow = domain.QuotaWindow{Limit: 100, Used: 20, Confidence: 1}
	healthy := makeAcc("healthy", "chatgpt", "t1")
	healthy.Quota.ShortWindow = domain.QuotaWindow{Limit: 100, Used: 20, Confidence: 1}
	healthy.Quota.LongWindow = domain.QuotaWindow{Limit: 100, Used: 20, Confidence: 1}
	s.Register(draining)
	s.Register(healthy)

	slot, err := s.Pick(context.Background(), PickRequest{Provider: "chatgpt", TenantID: "t1"})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if slot.Account.ID != "healthy" {
		t.Fatalf("new conversation should avoid draining account, got %s", slot.Account.ID)
	}
	if !s.AccountDraining("draining") {
		t.Fatal("expected low quota account to be draining")
	}
}

func TestPreferredAccountCanUseDrainingQuotaForStickyConversation(t *testing.T) {
	cfg := defaultCfg()
	cfg.Quota.DrainThreshold = 0.10
	s := New(cfg)
	draining := makeAcc("draining", "chatgpt", "t1")
	draining.Quota.ShortWindow = domain.QuotaWindow{Limit: 100, Used: 95, Confidence: 1}
	draining.Quota.LongWindow = domain.QuotaWindow{Limit: 100, Used: 20, Confidence: 1}
	healthy := makeAcc("healthy", "chatgpt", "t1")
	healthy.Quota.ShortWindow = domain.QuotaWindow{Limit: 100, Used: 20, Confidence: 1}
	healthy.Quota.LongWindow = domain.QuotaWindow{Limit: 100, Used: 20, Confidence: 1}
	s.Register(draining)
	s.Register(healthy)

	slot, err := s.Pick(context.Background(), PickRequest{
		Provider:           "chatgpt",
		TenantID:           "t1",
		PreferredAccountID: "draining",
	})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if slot.Account.ID != "draining" {
		t.Fatalf("sticky conversation should be allowed to use draining account, got %s", slot.Account.ID)
	}
}

// TestNeverFailProbeRecovers: both accounts marked exhausted, never-fail probes
// and succeeds on the one that's "back".
func TestNeverFailProbeRecovers(t *testing.T) {
	s := New(defaultCfg())
	s.Register(makeAcc("a1", "chatgpt", "t1"))
	s.Register(makeAcc("a2", "chatgpt", "t1"))
	s.MarkFailure("a1", domain.ErrQuotaExhausted)
	s.MarkFailure("a2", domain.ErrQuotaExhausted)

	probeCalls := 0
	s.SetProbeFunc(func(ctx context.Context, accountID string) error {
		probeCalls++
		// a2 recovers on first probe, a1 doesn't.
		if accountID == "a2" {
			return nil
		}
		return errors.New("still down")
	})
	slot, err := s.Pick(context.Background(), PickRequest{Provider: "chatgpt", TenantID: "t1"})
	if err != nil {
		t.Fatalf("expected probe-driven recovery, got %v", err)
	}
	if slot.Account.ID != "a2" {
		t.Errorf("expected a2 (the recoverable one), got %s", slot.Account.ID)
	}
	if probeCalls == 0 {
		t.Errorf("probe was never invoked")
	}
}

// TestNeverFailReturnsErrorWhenDisabled: with NeverFail disabled, exhausted pool
// returns error instead of probing.
func TestNeverFailReturnsErrorWhenDisabled(t *testing.T) {
	cfg := defaultCfg()
	cfg.Failover.NeverFail.Enabled = false
	s := New(cfg)
	s.Register(makeAcc("a1", "chatgpt", "t1"))
	s.MarkFailure("a1", domain.ErrQuotaExhausted)
	_, err := s.Pick(context.Background(), PickRequest{Provider: "chatgpt", TenantID: "t1"})
	if err == nil {
		t.Errorf("expected error when never-fail disabled and pool exhausted")
	}
}

// TestBreakerOpensThenRecovers: 3 quick failures open breaker, after duration
// the prober closes it.
func TestBreakerOpensThenRecovers(t *testing.T) {
	cfg := defaultCfg()
	cfg.Breaker.OpenDuration = 100 * time.Millisecond
	s := New(cfg)
	s.Register(makeAcc("a1", "chatgpt", "t1"))
	s.MarkFailure("a1", domain.ErrUpstreamError)
	s.MarkFailure("a1", domain.ErrUpstreamError)
	s.MarkFailure("a1", domain.ErrUpstreamError)

	slots := s.candidates(PickRequest{Provider: "chatgpt", TenantID: "t1"})
	if len(slots) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(slots))
	}
	if filterUsable(slots, PickRequest{}) != nil && len(filterUsable(slots, PickRequest{})) > 0 {
		t.Errorf("expected breaker to remove from usable")
	}
	time.Sleep(200 * time.Millisecond)
	s.MarkSuccess("a1", 50)
	if filterUsable(s.candidates(PickRequest{Provider: "chatgpt", TenantID: "t1"}), PickRequest{}) == nil {
		t.Errorf("expected recovery after success")
	}
}

func TestBannedAccountIsExcludedAndHooked(t *testing.T) {
	cfg := defaultCfg()
	cfg.Failover.NeverFail.Enabled = false
	s := New(cfg)
	s.Register(makeAcc("a1", "chatgpt", "t1"))
	hooked := make(chan string, 1)
	s.SetBannedHook(func(accountID string) {
		hooked <- accountID
	})

	s.MarkFailure("a1", domain.ErrBanned)

	select {
	case got := <-hooked:
		if got != "a1" {
			t.Fatalf("hook account: got %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("banned hook was not called")
	}

	_, err := s.Pick(context.Background(), PickRequest{Provider: "chatgpt", TenantID: "t1"})
	if err == nil {
		t.Fatal("banned account should not be picked while awaiting removal")
	}
}

func TestBannedAccountExcludedFromModelViews(t *testing.T) {
	s := New(defaultCfg())
	banned := makeAcc("banned", "chatgpt", "t1")
	banned.State = domain.StateBanned
	banned.Quota.DiscoveredModels = []domain.ModelCapability{{ID: "only-banned-model", Available: true}}
	active := makeAcc("active", "chatgpt", "t1")
	active.Quota.DiscoveredModels = []domain.ModelCapability{{ID: "active-model", Available: true}}
	s.Register(banned)
	s.Register(active)

	models := s.AvailableModels("chatgpt")
	for _, m := range models {
		if m.ID == "only-banned-model" {
			t.Fatal("banned account model should not be advertised")
		}
	}

	slot := s.BestAccountForModel("chatgpt", "only-banned-model")
	if slot != nil {
		t.Fatalf("banned account should not be selected for model-specific view, got %s", slot.Account.ID)
	}
}
