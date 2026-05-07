// Package metrics defines the Prometheus collectors used across the gateway.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	RequestTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pool_request_total",
		Help: "Total inbound requests routed by the gateway.",
	}, []string{"group", "provider", "model", "status"})

	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pool_request_duration_seconds",
		Help:    "Inbound request duration.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
	}, []string{"group", "provider", "model"})

	AccountState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pool_account_state",
		Help: "1 when account is in the labeled state, 0 otherwise.",
	}, []string{"account_id", "state"})

	AccountEWMA = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pool_account_ewma_latency_ms",
		Help: "Exponentially-weighted moving average latency in milliseconds.",
	}, []string{"account_id"})

	AccountInflight = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pool_account_inflight",
		Help: "Concurrent in-flight requests against the account.",
	}, []string{"account_id"})

	BreakerOpens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pool_breaker_opens_total",
		Help: "Number of times the breaker opened for this account.",
	}, []string{"account_id"})

	CFChallenges = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pool_cf_challenges_total",
		Help: "Number of Cloudflare challenges encountered.",
	}, []string{"account_id"})

	TranslationErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pool_translation_errors_total",
		Help: "Number of protocol translation errors.",
	}, []string{"from", "to"})

	FailoverSilent = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pool_failover_silent_total",
		Help: "Failovers that were transparent to downstream (Phase A or B).",
	}, []string{"phase"})

	FailoverVisible = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pool_failover_visible_total",
		Help: "Failovers that affected downstream (Phase C).",
	}, []string{"reason"})

	NeverFailInvoked = promauto.NewCounter(prometheus.CounterOpts{
		Name: "pool_never_fail_invoked_total",
		Help: "Number of times the never-fail decision path triggered.",
	})

	ProbeTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pool_probe_total",
		Help: "Account probes by result.",
	}, []string{"account_id", "result"})

	CacheStickyHit = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pool_cache_sticky_hit_total",
		Help: "Sticky-routing hits keeping a conversation on the same account.",
	}, []string{"group"})

	CacheReplay = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pool_cache_replay_total",
		Help: "Times a conversation had to be fully replayed to a new account.",
	}, []string{"group"})

	TokenSaved = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pool_token_saved_total",
		Help: "Estimated tokens saved by an optimisation.",
	}, []string{"group", "type"})
)
