// Package scheduler chooses an Account to serve a given request, applying
// the confidence model, sticky-routing, P2C+EWMA selection, breaker logic
// and the "never-fail" fallback path that probes cooling accounts rather
// than returning 5xx to downstream.
package scheduler

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/domain"
)

type Scheduler struct {
	cfg config.Scheduler
	mu  sync.RWMutex

	// accounts indexed by id; hot state lives here, cold state syncs to store.
	accounts map[string]*Slot

	// stickyMap: convHash -> accountID for cache-affinity routing.
	stickyMap map[string]stickyEntry
	stickyMu  sync.Mutex

	// prefixMap: systemHash -> accountID for cross-user prefix KV cache sharing.
	// Multiple users with the same system prompt (e.g. Claude Code) are routed
	// to the same account so the upstream prefix cache is reused across users.
	prefixMap map[string]prefixEntry
	prefixMu  sync.Mutex

	// probe is invoked by the never-fail path / background prober. Set by gateway.
	probe ProbeFunc

	bannedMu sync.RWMutex
	onBanned func(accountID string)

	snapshotCache   []SlotView
	snapshotCacheAt time.Time
	snapshotMu      sync.Mutex
}

type stickyEntry struct {
	accountID string
	expiresAt time.Time
}

type prefixEntry struct {
	accountID string
	lastUsed  time.Time
}

// ProbeFunc is supplied by the gateway. It calls provider.Probe with the right
// stealth client; returning nil means the account responded successfully.
type ProbeFunc func(ctx context.Context, accountID string) error

type Slot struct {
	// inflightAtomic is the hot-path inflight counter; use atomic ops to avoid
	// mutex contention at high concurrency. First field for cache-line isolation.
	inflightAtomic int64

	mu           sync.Mutex
	Account      *domain.Account
	EWMALatency  float64
	SuccessRate  float64
	Confidence   domain.AvailConfidence
	BreakerState domain.BreakerState
	OpenUntil    time.Time
	FailCount    int
	LastSuccess  time.Time
	LastFailure  time.Time
	NextProbeAt  time.Time
}

func New(cfg config.Scheduler) *Scheduler {
	return &Scheduler{
		cfg:       cfg,
		accounts:  make(map[string]*Slot),
		stickyMap: make(map[string]stickyEntry),
		prefixMap: make(map[string]prefixEntry),
	}
}

func (s *Scheduler) SetProbeFunc(f ProbeFunc) { s.probe = f }

// SetBannedHook registers a non-blocking callback fired when an account first
// transitions to StateBanned. The callback must tolerate duplicate calls.
func (s *Scheduler) SetBannedHook(f func(accountID string)) {
	s.bannedMu.Lock()
	s.onBanned = f
	s.bannedMu.Unlock()
}

func (s *Scheduler) notifyBanned(accountID string) {
	s.bannedMu.RLock()
	f := s.onBanned
	s.bannedMu.RUnlock()
	if f != nil {
		go f(accountID)
	}
}

func (s *Scheduler) RunGC(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.gcMaps()
		}
	}
}

func (s *Scheduler) gcMaps() {
	now := time.Now()
	s.stickyMu.Lock()
	for k, v := range s.stickyMap {
		if now.After(v.expiresAt) {
			delete(s.stickyMap, k)
		}
	}
	s.stickyMu.Unlock()

	cutoff := now.Add(-30 * time.Minute)
	s.prefixMu.Lock()
	for k, v := range s.prefixMap {
		if v.lastUsed.Before(cutoff) {
			delete(s.prefixMap, k)
		}
	}
	s.prefixMu.Unlock()
}

// Reload swaps the scheduler config (breaker thresholds, sticky TTL, etc.)
// without disrupting in-flight requests or losing account health state.
func (s *Scheduler) Reload(cfg config.Scheduler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}

// Register adds an account to the schedulable pool.
func (s *Scheduler) Register(a *domain.Account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.accounts[a.ID]; ok {
		existing.mu.Lock()
		existing.Account = a
		existing.mu.Unlock()
		return
	}
	s.accounts[a.ID] = &Slot{
		Account:    a,
		Confidence: domain.ConfLikelyAvailable,
	}
}

func (s *Scheduler) Unregister(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.accounts, id)
}

// AvailableModels returns the union of all discovered models across all
// accounts filtered by provider. Accounts with no discovered models are
// assumed to support any model (discovery pending).
// This is used by /v1/models and the admin UI to show real-time capabilities.
func (s *Scheduler) AvailableModels(provider string) []domain.ModelCapability {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]domain.ModelCapability{}
	for _, sl := range s.accounts {
		sl.mu.Lock()
		if !slotSelectableStateLocked(sl) || (provider != "" && sl.Account.Provider != provider) {
			sl.mu.Unlock()
			continue
		}
		for _, m := range sl.Account.Quota.DiscoveredModels {
			if m.Available {
				if _, exists := seen[m.ID]; !exists {
					seen[m.ID] = m
				}
			}
		}
		sl.mu.Unlock()
	}
	out := make([]domain.ModelCapability, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	return out
}

// BestAccountForModel picks the best account that supports the given model.
// Falls back to any usable account when no model-specific match exists
// (so discovery-pending accounts are still usable).
func (s *Scheduler) BestAccountForModel(provider, model string) *Slot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best *Slot
	var bestFallback *Slot
	for _, sl := range s.accounts {
		sl.mu.Lock()
		if !slotSelectableStateLocked(sl) {
			sl.mu.Unlock()
			continue
		}
		if sl.Account.Provider != provider {
			sl.mu.Unlock()
			continue
		}
		if sl.BreakerState == domain.BreakerOpen {
			sl.mu.Unlock()
			continue
		}
		if slotSupportsModelLocked(sl, model) {
			if best == nil || sl.EWMALatency < best.EWMALatency {
				best = sl
			}
		} else if len(sl.Account.Quota.DiscoveredModels) == 0 {
			// Discovery pending — may support this model, use as fallback.
			if bestFallback == nil || sl.EWMALatency < bestFallback.EWMALatency {
				bestFallback = sl
			}
		}
		sl.mu.Unlock()
	}
	if best != nil {
		return best
	}
	return bestFallback
}

// slotSupportsModelLocked checks model support; caller must hold sl.mu.
func slotSupportsModelLocked(sl *Slot, model string) bool {
	if len(sl.Account.Quota.DiscoveredModels) == 0 {
		return true // assume supported until discovered otherwise
	}
	for _, m := range sl.Account.Quota.DiscoveredModels {
		if m.ID == model && m.Available {
			return true
		}
	}
	return false
}

// Snapshot returns a shallow copy of all slots for admin/metrics.
func (s *Scheduler) Snapshot() []SlotView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SlotView, 0, len(s.accounts))
	now := time.Now()
	drainThreshold := s.quotaDrainThreshold()
	quotaPower := s.quotaCostPower()
	for _, sl := range s.accounts {
		sl.mu.Lock()
		out = append(out, slotViewLocked(sl, now, drainThreshold, quotaPower))
		sl.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccountID < out[j].AccountID })
	return out
}

func (s *Scheduler) SnapshotCached(ttl time.Duration) []SlotView {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	if time.Since(s.snapshotCacheAt) < ttl && s.snapshotCache != nil {
		return s.snapshotCache
	}
	s.snapshotCache = s.Snapshot()
	s.snapshotCacheAt = time.Now()
	return s.snapshotCache
}

type SlotView struct {
	AccountID  string
	Provider   string
	Email      string
	TenantID   string
	PlanTier   string
	State      string
	Confidence string
	Healthy    bool
	// StatusCategory/StatusLabel are the admin-facing account pool buckets.
	// SortRank/PickCost mirror scheduler preference: lower sorts earlier.
	StatusCategory string
	StatusLabel    string
	SortRank       int
	PickCost       float64
	Inflight       int
	EWMALatency    float64
	BreakerState   int
	OpenUntil      time.Time
	LastSuccess    time.Time
	LastFailure    time.Time
	// Quota mirrors domain.QuotaState for dashboard and CLI compat endpoints.
	QuotaShortUsed   float64
	QuotaShortLimit  float64
	QuotaShortReset  time.Time
	QuotaLongUsed    float64
	QuotaLongLimit   float64
	QuotaLongReset   time.Time
	DiscoveredModels []string
}

const (
	SlotStatusHealthy  = "healthy"
	SlotStatusLowQuota = "low_quota"
	SlotStatusNoQuota  = "no_quota"
	SlotStatusBanned   = "banned"
	SlotStatusAbnormal = "abnormal"
)

const (
	slotSortRankHealthy = iota
	slotSortRankLowQuota
	slotSortRankDegradedCandidate
	slotSortRankNoQuota
	slotSortRankBanned
	slotSortRankAbnormal
)

const maxSlotPickCost = 1e18

func SlotStatusLabel(category string) string {
	switch category {
	case SlotStatusHealthy:
		return "健康的"
	case SlotStatusLowQuota:
		return "额度低"
	case SlotStatusNoQuota:
		return "没有额度"
	case SlotStatusBanned:
		return "账号被封禁的"
	case SlotStatusAbnormal:
		return "账号异常的"
	default:
		return "账号异常的"
	}
}

func slotViewLocked(sl *Slot, now time.Time, drainThreshold, quotaPower float64) SlotView {
	category, rank, pickCost := slotDisplayStatusLocked(sl, now, drainThreshold, quotaPower)
	ms := make([]string, 0, len(sl.Account.Quota.DiscoveredModels))
	for _, m := range sl.Account.Quota.DiscoveredModels {
		ms = append(ms, m.ID)
	}
	return SlotView{
		AccountID:        sl.Account.ID,
		Provider:         sl.Account.Provider,
		Email:            sl.Account.Email,
		TenantID:         sl.Account.TenantID,
		PlanTier:         sl.Account.PlanTier,
		State:            string(sl.Account.State),
		Confidence:       sl.Confidence.String(),
		Healthy:          slotHealthyLocked(sl),
		StatusCategory:   category,
		StatusLabel:      SlotStatusLabel(category),
		SortRank:         rank,
		PickCost:         pickCost,
		Inflight:         sl.inflight(),
		EWMALatency:      sl.EWMALatency,
		BreakerState:     int(sl.BreakerState),
		OpenUntil:        sl.OpenUntil,
		LastSuccess:      sl.LastSuccess,
		LastFailure:      sl.LastFailure,
		QuotaShortUsed:   sl.Account.Quota.ShortWindow.Used,
		QuotaShortLimit:  sl.Account.Quota.ShortWindow.Limit,
		QuotaShortReset:  sl.Account.Quota.ShortWindow.ResetAt,
		QuotaLongUsed:    sl.Account.Quota.LongWindow.Used,
		QuotaLongLimit:   sl.Account.Quota.LongWindow.Limit,
		QuotaLongReset:   sl.Account.Quota.LongWindow.ResetAt,
		DiscoveredModels: ms,
	}
}

// SlotViewFromAccount returns a DB-only account view. It is intentionally
// sorted behind live scheduler slots because the next connection cannot pick an
// account that is not registered in the scheduler.
func SlotViewFromAccount(a *domain.Account) SlotView {
	if a == nil {
		return SlotView{
			StatusCategory: SlotStatusAbnormal,
			StatusLabel:    SlotStatusLabel(SlotStatusAbnormal),
			SortRank:       slotSortRankAbnormal,
			PickCost:       maxSlotPickCost,
		}
	}
	tmp := &Slot{Account: a, Confidence: domain.ConfLikelyAvailable}
	category, rank, pickCost := slotDisplayStatusLocked(tmp, time.Now(), defaultQuotaDrainThreshold, defaultQuotaCostPower)
	if category == SlotStatusHealthy || category == SlotStatusLowQuota {
		category = SlotStatusAbnormal
		rank = slotSortRankAbnormal
		pickCost = maxSlotPickCost
	}
	ms := make([]string, 0, len(a.Quota.DiscoveredModels))
	for _, m := range a.Quota.DiscoveredModels {
		ms = append(ms, m.ID)
	}
	return SlotView{
		AccountID:        a.ID,
		Provider:         a.Provider,
		Email:            a.Email,
		TenantID:         a.TenantID,
		PlanTier:         a.PlanTier,
		State:            string(a.State),
		Confidence:       domain.ConfLikelyAvailable.String(),
		StatusCategory:   category,
		StatusLabel:      SlotStatusLabel(category),
		SortRank:         rank,
		PickCost:         pickCost,
		QuotaShortUsed:   a.Quota.ShortWindow.Used,
		QuotaShortLimit:  a.Quota.ShortWindow.Limit,
		QuotaShortReset:  a.Quota.ShortWindow.ResetAt,
		QuotaLongUsed:    a.Quota.LongWindow.Used,
		QuotaLongLimit:   a.Quota.LongWindow.Limit,
		QuotaLongReset:   a.Quota.LongWindow.ResetAt,
		DiscoveredModels: ms,
	}
}

func slotDisplayStatusLocked(sl *Slot, now time.Time, drainThreshold, quotaPower float64) (string, int, float64) {
	if sl.Account.State == domain.StateBanned {
		return SlotStatusBanned, slotSortRankBanned, maxSlotPickCost
	}
	if !slotSelectableStateLocked(sl) {
		return SlotStatusAbnormal, slotSortRankAbnormal, maxSlotPickCost
	}

	quotaCandidate := quotaCandidateLocked(sl)
	if !quotaCandidate {
		return SlotStatusNoQuota, slotSortRankNoQuota, slotRecoverySortCostLocked(sl, now, quotaPower)
	}

	blockedByBreaker := sl.BreakerState == domain.BreakerOpen && now.Before(sl.OpenUntil)
	blockedByConfidence := sl.Confidence == domain.ConfCooling || sl.Confidence == domain.ConfProbablyExhausted
	blockedByState := sl.Account.State == domain.StateCooling || sl.Account.State == domain.StateCFChallenged
	if blockedByBreaker || blockedByConfidence || blockedByState {
		if sl.Confidence == domain.ConfProbablyExhausted {
			return SlotStatusNoQuota, slotSortRankDegradedCandidate, slotRecoverySortCostLocked(sl, now, quotaPower)
		}
		return SlotStatusAbnormal, slotSortRankDegradedCandidate, slotRecoverySortCostLocked(sl, now, quotaPower)
	}

	pickCost := slotCostLocked(sl, "", quotaPower)
	if quotaDrainingLocked(sl, drainThreshold) {
		return SlotStatusLowQuota, slotSortRankLowQuota, pickCost
	}
	return SlotStatusHealthy, slotSortRankHealthy, pickCost
}

func slotRecoverySortCostLocked(sl *Slot, now time.Time, quotaPower float64) float64 {
	if horizon := slotRecoveryHorizonLocked(sl); !horizon.IsZero() && horizon.After(now) {
		return float64(horizon.Sub(now).Milliseconds())
	}
	return slotCostLocked(sl, "", quotaPower)
}

func SortSlotViewsForPick(views []SlotView) {
	sort.SliceStable(views, func(i, j int) bool {
		if views[i].SortRank != views[j].SortRank {
			return views[i].SortRank < views[j].SortRank
		}
		if views[i].PickCost != views[j].PickCost {
			return views[i].PickCost < views[j].PickCost
		}
		return views[i].AccountID < views[j].AccountID
	})
}

// PickRequest carries the criteria and conversation hint for selecting an account.
type PickRequest struct {
	GroupID            string
	TenantID           string
	Provider           string
	Providers          []string // federation mode: multiple providers
	Model              string
	ConvHash           string   // optional, enables sticky routing
	SystemHash         string   // optional, enables cross-user prefix cache sharing
	AccountIDs         []string // restrict to these accounts (group binding)
	PreferredAccountID string   // optional, try this account first when still usable
	ExcludeAccountIDs  []string // optional, remove accounts that already failed this request
}

// Pick chooses the best account or, in degraded conditions, probes/waits.
// Never returns no-account error if NeverFail is enabled and there is at least
// one slot in the pool.
func (s *Scheduler) Pick(ctx context.Context, req PickRequest) (*Slot, error) {
	if req.PreferredAccountID != "" {
		s.mu.RLock()
		sl, exists := s.accounts[req.PreferredAccountID]
		s.mu.RUnlock()
		if exists && s.isUsable(sl, req) {
			s.recordSticky(req.ConvHash, req.PreferredAccountID)
			s.recordPrefix(req.SystemHash, req.PreferredAccountID)
			return sl, nil
		}
	}

	if entry, ok := s.lookupSticky(req.ConvHash); ok {
		s.mu.RLock()
		sl, exists := s.accounts[entry.accountID]
		s.mu.RUnlock()
		if exists && s.isUsable(sl, req) {
			return sl, nil
		}
	}

	// Cross-user prefix sharing: route users with the same system prompt
	// to the same account so upstream prefix KV cache is reused.
	if prefixAcct, ok := s.lookupPrefix(req.SystemHash); ok {
		s.mu.RLock()
		sl, exists := s.accounts[prefixAcct]
		s.mu.RUnlock()
		if exists && s.isUsable(sl, req) {
			s.recordSticky(req.ConvHash, prefixAcct)
			return sl, nil
		}
	}

	candidates := s.candidates(req)
	if len(candidates) == 0 && !s.cfg.Failover.NeverFail.Enabled {
		return nil, errors.New("no candidate accounts available")
	}

	usable := s.filterUsable(candidates, req)
	if len(usable) > 0 {
		picked := s.p2cSelect(usable, req.Model)
		s.recordSticky(req.ConvHash, picked.Account.ID)
		s.recordPrefix(req.SystemHash, picked.Account.ID)
		return picked, nil
	}

	if !s.cfg.Failover.NeverFail.Enabled {
		return nil, errors.New("no usable accounts (all unhealthy)")
	}

	return s.neverFail(ctx, candidates, req)
}

// HasUsable reports whether a request has at least one immediately selectable
// account without entering the never-fail probe/wait path.
func (s *Scheduler) HasUsable(req PickRequest) bool {
	candidates := s.candidates(req)
	if len(candidates) == 0 {
		return false
	}
	return len(s.filterUsable(candidates, req)) > 0
}

func (s *Scheduler) candidates(req PickRequest) []*Slot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*Slot{}
	for _, sl := range s.accounts {
		if !s.matchesRequest(sl, req) {
			continue
		}
		out = append(out, sl)
	}
	return out
}

func slotSupportsModel(sl *Slot, model string) bool {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	return slotSupportsModelLocked(sl, model)
}

func (s *Scheduler) filterUsable(slots []*Slot, req PickRequest) []*Slot {
	return filterUsableWithDrainThreshold(slots, req, s.quotaDrainThreshold())
}

func filterUsable(slots []*Slot, req PickRequest) []*Slot {
	return filterUsableWithDrainThreshold(slots, req, defaultQuotaDrainThreshold)
}

func filterUsableWithDrainThreshold(slots []*Slot, req PickRequest, drainThreshold float64) []*Slot {
	now := time.Now()
	usable := make([]*Slot, 0, len(slots))
	nonDraining := make([]*Slot, 0, len(slots))
	for _, sl := range slots {
		sl.mu.Lock()
		if sl.BreakerState == domain.BreakerOpen && now.Before(sl.OpenUntil) {
			sl.mu.Unlock()
			continue
		}
		if sl.Confidence == domain.ConfCooling || sl.Confidence == domain.ConfProbablyExhausted {
			sl.mu.Unlock()
			continue
		}
		if !quotaCandidateLocked(sl) {
			sl.mu.Unlock()
			continue
		}
		draining := quotaDrainingLocked(sl, drainThreshold)
		sl.mu.Unlock()
		usable = append(usable, sl)
		if !draining {
			nonDraining = append(nonDraining, sl)
		}
	}
	if len(nonDraining) > 0 {
		return nonDraining
	}
	return usable
}

// p2cSelect implements Power-of-Two-Choices: pick two random candidates and
// take the one with the lower cost = ewma_latency * (1 + inflight) / quota_factor.
func (s *Scheduler) p2cSelect(slots []*Slot, model string) *Slot {
	if len(slots) == 1 {
		return slots[0]
	}
	a := pickRand(slots)
	b := pickRand(slots)
	for i := 0; i < 5 && a == b && len(slots) > 1; i++ {
		b = pickRand(slots)
	}
	power := s.quotaCostPower()
	if cost(a, model, power) <= cost(b, model, power) {
		return a
	}
	return b
}

func pickRand(slots []*Slot) *Slot {
	n := big.NewInt(int64(len(slots)))
	r, err := rand.Int(rand.Reader, n)
	if err != nil {
		return slots[0]
	}
	return slots[int(r.Int64())]
}

func cost(sl *Slot, model string, quotaPower float64) float64 {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	return slotCostLocked(sl, model, quotaPower)
}

func slotCostLocked(sl *Slot, model string, quotaPower float64) float64 {
	lat := sl.EWMALatency
	if lat <= 0 {
		lat = 100
	}
	qFactor := quotaRemainingFactorLocked(sl, model)
	if qFactor < 0.05 {
		qFactor = 0.05
	}
	if quotaPower > 1 {
		qFactor = math.Pow(qFactor, quotaPower)
		if qFactor < 0.0025 {
			qFactor = 0.0025
		}
	}
	return lat * float64(1+sl.inflight()) / qFactor
}

// modelToTierKey maps a model name to its TierWindows key.
func modelToTierKey(model string) string {
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "opus"):
		return "seven_day_opus"
	case strings.Contains(lower, "sonnet"):
		return "seven_day_sonnet"
	case strings.Contains(lower, "flash-lite") || strings.Contains(lower, "flash_lite"):
		return "gemini_flash_lite"
	case strings.Contains(lower, "flash"):
		return "gemini_flash"
	case strings.Contains(lower, "gemini") && strings.Contains(lower, "pro"):
		return "gemini_pro"
	}
	return ""
}

// neverFail is the "绝不放弃" path: when no candidates are immediately usable,
// pick the one with the soonest expected recovery and probe it. Each cycle we
// try every candidate in recovery-horizon order so a single hard-down account
// can't block the entire pool.
func (s *Scheduler) neverFail(ctx context.Context, slots []*Slot, req PickRequest) (*Slot, error) {
	if len(slots) == 0 {
		return nil, errors.New("never-fail: no slot exists in pool at all")
	}
	deadline := time.Now().Add(s.cfg.Failover.NeverFail.MaxWait)
	probeInterval := s.cfg.Failover.NeverFail.ProbeInterval
	if probeInterval == 0 {
		probeInterval = 30 * time.Second
	}
	if s.probe == nil {
		return nil, fmt.Errorf("never-fail: probe func not set")
	}
	for time.Now().Before(deadline) {
		sort.Slice(slots, func(i, j int) bool {
			return slotRecoveryHorizon(slots[i]).Before(slotRecoveryHorizon(slots[j]))
		})
		for _, target := range slots {
			probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			err := s.probe(probeCtx, target.Account.ID)
			cancel()
			if err == nil {
				s.markSuccess(target, 200)
				return target, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(probeInterval):
		}
	}
	return nil, errors.New("never-fail: max_wait exceeded")
}

func slotRecoveryHorizon(sl *Slot) time.Time {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	return slotRecoveryHorizonLocked(sl)
}

func slotRecoveryHorizonLocked(sl *Slot) time.Time {
	if !sl.OpenUntil.IsZero() {
		return sl.OpenUntil
	}
	return sl.Account.Quota.ShortWindow.ResetAt
}

func (s *Scheduler) isUsable(sl *Slot, req PickRequest) bool {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	now := time.Now()
	if !matchesRequestLocked(sl, req) {
		return false
	}
	if sl.BreakerState == domain.BreakerOpen && now.Before(sl.OpenUntil) {
		return false
	}
	if sl.Confidence == domain.ConfCooling || sl.Confidence == domain.ConfProbablyExhausted {
		return false
	}
	if !quotaCandidateLocked(sl) {
		return false
	}
	return true
}

func (s *Scheduler) matchesRequest(sl *Slot, req PickRequest) bool {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	return matchesRequestLocked(sl, req)
}

func matchesRequestLocked(sl *Slot, req PickRequest) bool {
	if !slotSelectableStateLocked(sl) {
		return false
	}
	if !accountAllowed(sl.Account.ID, req.AccountIDs) {
		return false
	}
	if accountAllowed(sl.Account.ID, req.ExcludeAccountIDs) && len(req.ExcludeAccountIDs) > 0 {
		return false
	}
	if req.TenantID != "" && sl.Account.TenantID != req.TenantID {
		return false
	}
	if !providerMatches(sl.Account.Provider, req) {
		return false
	}
	if req.Model != "" && !slotSupportsModelLocked(sl, req.Model) {
		return false
	}
	if !quotaCandidateLocked(sl) {
		return false
	}
	return true
}

func slotSelectableStateLocked(sl *Slot) bool {
	switch sl.Account.State {
	case domain.StateBanned, domain.StateDisabled:
		return false
	default:
		return true
	}
}

func accountAllowed(id string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if a == id {
			return true
		}
	}
	return false
}

func providerMatches(accProvider string, req PickRequest) bool {
	if req.Provider != "" {
		return accProvider == req.Provider
	}
	if len(req.Providers) > 0 {
		for _, p := range req.Providers {
			if p == accProvider {
				return true
			}
		}
		return false
	}
	return true
}

func quotaCandidateLocked(sl *Slot) bool {
	q := sl.Account.Quota
	shortKnown := quotaWindowKnown(q.ShortWindow)
	longKnown := quotaWindowKnown(q.LongWindow)
	if shortKnown && !quotaWindowHasRemaining(q.ShortWindow) {
		return false
	}
	if longKnown && !quotaWindowHasRemaining(q.LongWindow) {
		return false
	}
	if shortKnown && !longKnown {
		return false
	}
	return true
}

const (
	defaultQuotaDrainThreshold = 0.10
	defaultQuotaCostPower      = 2.0
)

func (s *Scheduler) quotaDrainThreshold() float64 {
	threshold := s.cfg.Quota.DrainThreshold
	if threshold <= 0 {
		return defaultQuotaDrainThreshold
	}
	if threshold >= 1 {
		return 1
	}
	return threshold
}

func (s *Scheduler) quotaCostPower() float64 {
	power := s.cfg.Quota.CostPower
	if power <= 0 {
		return defaultQuotaCostPower
	}
	if power > 6 {
		return 6
	}
	return power
}

func quotaWindowKnown(w domain.QuotaWindow) bool {
	return w.Limit > 0 || w.Confidence > 0 || !w.ResetAt.IsZero()
}

func quotaWindowHasRemaining(w domain.QuotaWindow) bool {
	return w.Limit > 0 && w.Used < w.Limit
}

func quotaDrainingLocked(sl *Slot, threshold float64) bool {
	if threshold <= 0 {
		threshold = defaultQuotaDrainThreshold
	}
	if threshold >= 1 {
		threshold = 1
	}
	if !quotaCandidateLocked(sl) {
		return true
	}
	return quotaRemainingFactorLocked(sl, "") <= threshold
}

func quotaRemainingFactorLocked(sl *Slot, model string) float64 {
	q := sl.Account.Quota
	factor := 1.0
	seen := false
	if tw := q.TierWindows; len(tw) > 0 && model != "" {
		tierKey := modelToTierKey(model)
		if tierKey != "" {
			if w, ok := tw[tierKey]; ok && w.Limit > 0 {
				factor = minFloat(factor, quotaWindowRemainingFraction(w))
				seen = true
			}
		}
	}
	if q.ShortWindow.Limit > 0 {
		factor = minFloat(factor, quotaWindowRemainingFraction(q.ShortWindow))
		seen = true
	}
	if q.LongWindow.Limit > 0 {
		factor = minFloat(factor, quotaWindowRemainingFraction(q.LongWindow))
		seen = true
	}
	if !seen {
		return 1
	}
	return factor
}

func quotaWindowRemainingFraction(w domain.QuotaWindow) float64 {
	if w.Limit <= 0 {
		return 0
	}
	rem := (w.Limit - w.Used) / w.Limit
	if rem < 0 {
		return 0
	}
	if rem > 1 {
		return 1
	}
	return rem
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func slotHealthyLocked(sl *Slot) bool {
	if !slotSelectableStateLocked(sl) {
		return false
	}
	if sl.BreakerState != domain.BreakerClosed {
		return false
	}
	if sl.Confidence != domain.ConfConfirmedAvailable && sl.Confidence != domain.ConfLikelyAvailable {
		return false
	}
	return quotaCandidateLocked(sl)
}

func (s *Scheduler) lookupSticky(convHash string) (stickyEntry, bool) {
	if convHash == "" || !s.cfg.Sticky.Enabled {
		return stickyEntry{}, false
	}
	s.stickyMu.Lock()
	defer s.stickyMu.Unlock()
	e, ok := s.stickyMap[convHash]
	if !ok {
		return e, false
	}
	if time.Now().After(e.expiresAt) {
		delete(s.stickyMap, convHash)
		return stickyEntry{}, false
	}
	return e, true
}

func (s *Scheduler) lookupPrefix(systemHash string) (string, bool) {
	if systemHash == "" {
		return "", false
	}
	s.prefixMu.Lock()
	defer s.prefixMu.Unlock()
	e, ok := s.prefixMap[systemHash]
	if ok {
		e.lastUsed = time.Now()
		s.prefixMap[systemHash] = e
	}
	return e.accountID, ok
}

func (s *Scheduler) recordPrefix(systemHash, accountID string) {
	if systemHash == "" {
		return
	}
	s.prefixMu.Lock()
	defer s.prefixMu.Unlock()
	if len(s.prefixMap) >= 4096 {
		s.evictPrefixLocked()
	}
	s.prefixMap[systemHash] = prefixEntry{accountID: accountID, lastUsed: time.Now()}
}

func (s *Scheduler) evictPrefixLocked() {
	cutoff := time.Now().Add(-30 * time.Minute)
	for k, v := range s.prefixMap {
		if v.lastUsed.Before(cutoff) {
			delete(s.prefixMap, k)
		}
	}
	if len(s.prefixMap) >= 4096 {
		var oldest string
		var oldestT time.Time
		for k, v := range s.prefixMap {
			if oldest == "" || v.lastUsed.Before(oldestT) {
				oldest = k
				oldestT = v.lastUsed
			}
		}
		if oldest != "" {
			delete(s.prefixMap, oldest)
		}
	}
}

func (s *Scheduler) recordSticky(convHash, accountID string) {
	if convHash == "" || !s.cfg.Sticky.Enabled {
		return
	}
	s.stickyMu.Lock()
	defer s.stickyMu.Unlock()
	ttl := s.cfg.Sticky.StickyTTL
	if ttl == 0 {
		ttl = 30 * time.Minute
	}
	s.stickyMap[convHash] = stickyEntry{accountID: accountID, expiresAt: time.Now().Add(ttl)}
}

// MarkSuccess updates EWMA and resets failure counters.
func (s *Scheduler) MarkSuccess(accountID string, latencyMs float64) {
	s.mu.RLock()
	sl, ok := s.accounts[accountID]
	s.mu.RUnlock()
	if !ok {
		return
	}
	s.markSuccess(sl, latencyMs)
}

// UpdateQuota writes a fresh QuotaState (including DiscoveredModels) back into
// the in-memory slot. Called by DiscoveryRunner and the admin probe handler.
func (s *Scheduler) UpdateQuota(accountID string, q *domain.QuotaState) {
	if q == nil {
		return
	}
	s.mu.RLock()
	sl, ok := s.accounts[accountID]
	s.mu.RUnlock()
	if !ok {
		return
	}
	sl.mu.Lock()
	sl.Account.Quota = *q
	if q.PlanTier != "" {
		sl.Account.PlanTier = q.PlanTier
	}
	if quotaCandidateLocked(sl) {
		sl.Confidence = domain.ConfConfirmedAvailable
	} else {
		sl.Confidence = domain.ConfProbablyExhausted
	}
	sl.mu.Unlock()
}

func (s *Scheduler) AccountDraining(accountID string) bool {
	if accountID == "" {
		return false
	}
	s.mu.RLock()
	sl, ok := s.accounts[accountID]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	sl.mu.Lock()
	defer sl.mu.Unlock()
	return quotaDrainingLocked(sl, s.quotaDrainThreshold())
}

func (s *Scheduler) markSuccess(sl *Slot, latencyMs float64) {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	alpha := s.cfg.EWMAAlpha
	if alpha == 0 {
		alpha = 0.3
	}
	if sl.EWMALatency == 0 {
		sl.EWMALatency = latencyMs
	} else {
		sl.EWMALatency = alpha*latencyMs + (1-alpha)*sl.EWMALatency
	}
	sl.LastSuccess = time.Now()
	sl.FailCount = 0
	sl.BreakerState = domain.BreakerClosed
	sl.OpenUntil = time.Time{}
	sl.Confidence = domain.ConfConfirmedAvailable
}

// MarkFailure attributes a failure with classification, possibly tripping the breaker.
func (s *Scheduler) MarkFailure(accountID string, class domain.ErrorClass) {
	s.mu.RLock()
	sl, ok := s.accounts[accountID]
	s.mu.RUnlock()
	if !ok {
		return
	}
	sl.mu.Lock()
	wasBanned := sl.Account.State == domain.StateBanned
	sl.LastFailure = time.Now()
	sl.FailCount++
	becameBanned := false

	switch class {
	case domain.ErrBanned:
		sl.Account.State = domain.StateBanned
		sl.Confidence = domain.ConfProbablyExhausted
		sl.BreakerState = domain.BreakerOpen
		sl.OpenUntil = time.Now().Add(365 * 24 * time.Hour) // permanently open
		becameBanned = !wasBanned
	case domain.ErrQuotaExhausted:
		sl.Confidence = domain.ConfProbablyExhausted
		sl.BreakerState = domain.BreakerOpen
		sl.OpenUntil = time.Now().Add(s.openDuration(sl))
	case domain.ErrRateLimited:
		sl.Confidence = domain.ConfSuspectedIssue
		if sl.FailCount >= s.cfg.Breaker.FailThreshold {
			sl.BreakerState = domain.BreakerOpen
			sl.OpenUntil = time.Now().Add(s.openDuration(sl))
		}
	case domain.ErrCFChallenge:
		sl.Account.State = domain.StateCFChallenged
		sl.BreakerState = domain.BreakerOpen
		sl.OpenUntil = time.Now().Add(s.openDuration(sl))
	case domain.ErrAuthFailed:
		sl.Confidence = domain.ConfSuspectedIssue
		sl.BreakerState = domain.BreakerOpen
		sl.OpenUntil = time.Now().Add(s.openDuration(sl))
	default:
		if sl.FailCount >= s.cfg.Breaker.FailThreshold {
			sl.Confidence = domain.ConfSuspectedIssue
			sl.BreakerState = domain.BreakerOpen
			sl.OpenUntil = time.Now().Add(s.openDuration(sl))
		}
	}
	sl.mu.Unlock()
	if becameBanned {
		s.notifyBanned(accountID)
	}
}

func (s *Scheduler) openDuration(sl *Slot) time.Duration {
	d := s.cfg.Breaker.OpenDuration
	if d == 0 {
		d = 60 * time.Second
	}
	if s.cfg.Breaker.Backoff == "exponential" && sl.FailCount > 1 {
		mult := time.Duration(1)
		for i := 1; i < sl.FailCount && i < 8; i++ {
			mult *= 2
		}
		d *= mult
	}
	if s.cfg.Breaker.OpenMax > 0 && d > s.cfg.Breaker.OpenMax {
		d = s.cfg.Breaker.OpenMax
	}
	return d
}

// IncInflight / DecInflight track concurrent calls to the same account.
func (s *Scheduler) IncInflight(accountID string) {
	s.mu.RLock()
	sl, ok := s.accounts[accountID]
	s.mu.RUnlock()
	if !ok {
		return
	}
	atomic.AddInt64(&sl.inflightAtomic, 1)
}

func (s *Scheduler) DecInflight(accountID string) {
	s.mu.RLock()
	sl, ok := s.accounts[accountID]
	s.mu.RUnlock()
	if !ok {
		return
	}
	for {
		cur := atomic.LoadInt64(&sl.inflightAtomic)
		if cur <= 0 {
			return
		}
		if atomic.CompareAndSwapInt64(&sl.inflightAtomic, cur, cur-1) {
			return
		}
	}
}

// inflight returns the current inflight count for a slot (atomic read).
func (sl *Slot) inflight() int {
	return int(atomic.LoadInt64(&sl.inflightAtomic))
}

// AccountByID exposes a slot for direct manipulation (used by gateway during invocation).
func (s *Scheduler) AccountByID(id string) (*domain.Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sl, ok := s.accounts[id]
	if !ok {
		return nil, false
	}
	sl.mu.Lock()
	a := sl.Account
	sl.mu.Unlock()
	return a, true
}

// BannedAccountIDs returns IDs of all accounts in StateBanned.
func (s *Scheduler) BannedAccountIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var ids []string
	for _, sl := range s.accounts {
		sl.mu.Lock()
		if sl.Account.State == domain.StateBanned {
			ids = append(ids, sl.Account.ID)
		}
		sl.mu.Unlock()
	}
	return ids
}
