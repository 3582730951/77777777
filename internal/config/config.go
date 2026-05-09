package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Root struct {
	Server         Server         `yaml:"server"`
	Resource       Resource       `yaml:"resource"`
	Tenants        []Tenant       `yaml:"tenants"`
	Groups         []Group        `yaml:"groups"`
	Federations    []Federation   `yaml:"federations"`
	AutoReg        AutoReg        `yaml:"autoreg"`
	Scheduler      Scheduler      `yaml:"scheduler"`
	Stealth        Stealth        `yaml:"stealth"`
	Discovery      Discovery      `yaml:"discovery"`
	Cluster        Cluster        `yaml:"cluster"`
	Storage        Storage        `yaml:"storage"`
	Redis          Redis          `yaml:"redis"`
	Logging        Logging        `yaml:"logging"`
	Portal         Portal         `yaml:"portal"`
	TokenOptimizer TokenOptimizer `yaml:"token_optimizer"`
}

// Portal controls user self-registration behavior.
type Portal struct {
	// InviteCode: if non-empty, users must enter this code to register.
	// Leave empty to allow open registration (not recommended for public deployments).
	InviteCode string `yaml:"invite_code"`
	// DefaultTenant: tenant_id assigned to self-registered users. Defaults to "default".
	DefaultTenant string `yaml:"default_tenant"`
	// AllowRegister: set to false to disable self-registration entirely.
	AllowRegister *bool `yaml:"allow_register"`
}

type Server struct {
	GatewayAddr               string        `yaml:"gateway_addr"`
	AdminAddr                 string        `yaml:"admin_addr"`
	ReadTimeout               time.Duration `yaml:"read_timeout"`
	ReadHeaderTimeout         time.Duration `yaml:"read_header_timeout"`
	WriteTimeout              time.Duration `yaml:"write_timeout"`
	IdleTimeout               time.Duration `yaml:"idle_timeout"`
	MaxHeaderBytes            int           `yaml:"max_header_bytes"`
	MaxRequestBytes           int64         `yaml:"max_request_bytes"`
	RequestBodyMemoryBudget   int64         `yaml:"request_body_memory_budget"`
	NetworkIngressBytesPerSec int64         `yaml:"network_ingress_bytes_per_sec"`
	NetworkEgressBytesPerSec  int64         `yaml:"network_egress_bytes_per_sec"`
	NetworkBurstBytes         int64         `yaml:"network_burst_bytes"`
	RateLimitRPM              float64       `yaml:"rate_limit_rpm"`
	RateLimitBurst            int           `yaml:"rate_limit_burst"`
}

type Resource struct {
	Profile              string        `yaml:"profile"`
	GoMemLimit           string        `yaml:"go_mem_limit"`
	GoGC                 int           `yaml:"go_gc"`
	SQLiteMmap           string        `yaml:"sqlite_mmap"`
	ConversationCacheMax int           `yaml:"conversation_cache_max"`
	ResponsesStateMax    int           `yaml:"responses_state_max"`
	ResponsesStateBytes  int64         `yaml:"responses_state_bytes"`
	ResponsesStateTTL    time.Duration `yaml:"responses_state_ttl"`
	BufferPoolMax        int           `yaml:"buffer_pool_max"`
	Chromium             struct {
		Lazy          bool          `yaml:"lazy"`
		IdleTTL       time.Duration `yaml:"idle_ttl"`
		MaxConcurrent int           `yaml:"max_concurrent"`
	} `yaml:"chromium"`
}

type Tenant struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
}

type Group struct {
	ID               string                    `yaml:"id"`
	TenantID         string                    `yaml:"tenant_id"`
	Provider         string                    `yaml:"provider"`
	APIKeys          []string                  `yaml:"api_keys"`
	Models           []string                  `yaml:"models"`
	ModelAliases     map[string]string         `yaml:"model_aliases"`
	ModelWhitelist   []string                  `yaml:"model_whitelist"`
	APIKeyOverrides  map[string]APIKeyOverride `yaml:"apikey_overrides"`
	AccountIDs       []string                  `yaml:"account_ids"`
	SystemPrompt     string                    `yaml:"system_prompt"`
	SystemPromptMode string                    `yaml:"system_prompt_mode"`
	// SystemPromptInjection controls when ChatGPT Responses passthrough injects
	// SystemPrompt. Empty/"always" preserves the current behavior.
	// "thread_once" injects once per known ChatGPT Responses thread, then
	// reinjects after compact or account replay.
	SystemPromptInjection string `yaml:"system_prompt_injection"`
	SourcePlatform        string `yaml:"source_platform"`
	AutoRegister          bool   `yaml:"auto_register"`
}

type Federation struct {
	ID           string   `yaml:"id"`
	Description  string   `yaml:"description"`
	MemberGroups []string `yaml:"member_groups"`
	APIKeys      []string `yaml:"api_keys"`
}

type AutoReg struct {
	Enabled                  bool              `yaml:"enabled"`
	PythonPath               string            `yaml:"python_path"`
	WorkDir                  string            `yaml:"work_dir"`
	Listen                   string            `yaml:"listen"`
	SyncInterval             time.Duration     `yaml:"sync_interval"`
	AutoActivate             bool              `yaml:"auto_activate"`
	AutoDiscovery            bool              `yaml:"auto_discovery"`
	DiscoveryRefreshInterval time.Duration     `yaml:"discovery_refresh_interval"`
	PlatformGroupMapping     map[string]string `yaml:"platform_group_mapping"`
}

type APIKeyOverride struct {
	ModelAliases map[string]string `yaml:"model_aliases"`
}

type Scheduler struct {
	Algorithm string  `yaml:"algorithm"`
	EWMAAlpha float64 `yaml:"ewma_alpha"`
	Breaker   struct {
		FailThreshold int           `yaml:"fail_threshold"`
		OpenDuration  time.Duration `yaml:"open_duration"`
		OpenMax       time.Duration `yaml:"open_max"`
		Backoff       string        `yaml:"backoff"`
	} `yaml:"breaker"`
	Retry struct {
		MaxAttempts int   `yaml:"max_attempts"`
		BackoffMs   []int `yaml:"backoff_ms"`
	} `yaml:"retry"`
	Sticky struct {
		Enabled        bool          `yaml:"enabled"`
		FallbackReplay string        `yaml:"fallback_replay"`
		StickyTTL      time.Duration `yaml:"sticky_ttl"`
	} `yaml:"sticky"`
	Failover struct {
		HeadBuffer struct {
			MaxDuration time.Duration `yaml:"max_duration"`
			MaxBytes    int           `yaml:"max_bytes"`
			MaxEvents   int           `yaml:"max_events"`
		} `yaml:"head_buffer"`
		MidStream struct {
			Mode              string `yaml:"mode"`
			MarkAccountOnFail string `yaml:"mark_account_on_fail"`
		} `yaml:"mid_stream"`
		NeverFail struct {
			Enabled          bool          `yaml:"enabled"`
			MaxWait          time.Duration `yaml:"max_wait"`
			ProbeInterval    time.Duration `yaml:"probe_interval"`
			ProbeCost        string        `yaml:"probe_cost"`
			DegradedResponse string        `yaml:"degraded_response"`
		} `yaml:"never_fail"`
	} `yaml:"failover"`
	Prober struct {
		BackgroundInterval time.Duration `yaml:"background_interval"`
		OnStateChange      string        `yaml:"on_state_change"`
	} `yaml:"prober"`
	Consistency struct {
		RequireSamePlanTier           bool `yaml:"require_same_plan_tier"`
		RequireSameModels             bool `yaml:"require_same_models"`
		FullHistoryReplayOnSwitch     bool `yaml:"full_history_replay_on_switch"`
		DisableAccountPersonalization bool `yaml:"disable_account_personalization"`
	} `yaml:"consistency"`
	Quota struct {
		DrainThreshold float64 `yaml:"drain_threshold"`
		CostPower      float64 `yaml:"cost_power"`
	} `yaml:"quota"`
}

type TokenOptimizer struct {
	Mode                        string `yaml:"mode"` // off | cleanup | guarded | safe | aggressive
	MinToolOutputBytes          int    `yaml:"min_tool_output_bytes"`
	MaxOptimizedToolOutputBytes int    `yaml:"max_optimized_tool_output_bytes"`
	HeadLines                   int    `yaml:"head_lines"`
	TailLines                   int    `yaml:"tail_lines"`
	ErrorContextLines           int    `yaml:"error_context_lines"`
}

func NormalizeTokenOptimizer(cfg TokenOptimizer) TokenOptimizer {
	if mode, ok := NormalizeTokenOptimizerMode(cfg.Mode); ok {
		cfg.Mode = mode
	} else {
		cfg.Mode = "off"
	}
	if cfg.MinToolOutputBytes <= 0 {
		cfg.MinToolOutputBytes = 32 << 10
	}
	if cfg.MaxOptimizedToolOutputBytes <= 0 {
		cfg.MaxOptimizedToolOutputBytes = 64 << 10
	}
	if cfg.HeadLines <= 0 {
		cfg.HeadLines = 80
	}
	if cfg.TailLines <= 0 {
		cfg.TailLines = 80
	}
	if cfg.ErrorContextLines < 0 {
		cfg.ErrorContextLines = 0
	}
	if cfg.ErrorContextLines == 0 {
		cfg.ErrorContextLines = 6
	}
	return cfg
}

func NormalizeTokenOptimizerMode(mode string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "off":
		return "off", true
	case "cleanup", "lossless":
		return "cleanup", true
	case "guarded":
		return "guarded", true
	case "safe":
		return "safe", true
	case "aggressive":
		return "aggressive", true
	default:
		return "", false
	}
}

type Stealth struct {
	DefaultTLSProfile string   `yaml:"default_tls_profile"`
	UserAgents        []string `yaml:"user_agents"`
	WarmupOnFirstUse  bool     `yaml:"warmup_on_first_use"`
	MaxRPSPerPersona  float64  `yaml:"max_rps_per_persona"`
	JitterPercent     int      `yaml:"jitter_percent"`
}

type Discovery struct {
	DefaultInterval  time.Duration `yaml:"default_interval"`
	StableInterval   time.Duration `yaml:"stable_interval"`
	UnstableInterval time.Duration `yaml:"unstable_interval"`
	OnFirstSeen      string        `yaml:"on_first_seen"`
}

type Cluster struct {
	Enabled      bool          `yaml:"enabled"`
	Identity     string        `yaml:"identity"`
	Region       string        `yaml:"region"`
	Peers        []ClusterPeer `yaml:"peers"`
	PullInterval time.Duration `yaml:"pull_interval"`
	PullTimeout  time.Duration `yaml:"pull_timeout"`
}

type ClusterPeer struct {
	Name  string `yaml:"name"`
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
}

type Storage struct {
	DBPath     string `yaml:"db_path"`
	PasswdPath string `yaml:"passwd_path"`
	MasterKey  string `yaml:"-"`
}

type Redis struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

type Logging struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

func Load(path string) (*Root, error) {
	if path == "" {
		path = "config/config.yaml"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return defaults(), nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	r := defaults()
	if err := yaml.Unmarshal(b, r); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	r.Storage.MasterKey = os.Getenv("POOL_MASTER_KEY")
	return r, nil
}

func defaults() *Root {
	r := &Root{}
	r.Server.GatewayAddr = ":8787"
	r.Server.AdminAddr = ":8788"
	r.Server.ReadTimeout = 300 * time.Second
	r.Server.ReadHeaderTimeout = 10 * time.Second
	r.Server.WriteTimeout = 600 * time.Second
	r.Server.IdleTimeout = 120 * time.Second
	r.Server.MaxHeaderBytes = 1 << 20
	r.Server.MaxRequestBytes = 512 << 20
	r.Server.RequestBodyMemoryBudget = 512 << 20
	r.Server.NetworkIngressBytesPerSec = 8 << 20
	r.Server.NetworkEgressBytesPerSec = 4 << 20
	r.Server.NetworkBurstBytes = 1 << 20
	r.Server.RateLimitRPM = 300
	r.Server.RateLimitBurst = 60

	r.Resource.Profile = "lowmem"
	r.Resource.GoMemLimit = "768MiB"
	r.Resource.GoGC = 50
	r.Resource.SQLiteMmap = "30MB"
	r.Resource.ConversationCacheMax = 10000
	r.Resource.ResponsesStateMax = 5000
	r.Resource.ResponsesStateBytes = 256 << 20
	r.Resource.ResponsesStateTTL = 12 * time.Hour
	r.Resource.BufferPoolMax = 1000
	r.Resource.Chromium.Lazy = true
	r.Resource.Chromium.IdleTTL = 5 * time.Minute
	r.Resource.Chromium.MaxConcurrent = 1

	r.Scheduler.Algorithm = "p2c_ewma"
	r.Scheduler.EWMAAlpha = 0.3
	r.Scheduler.Breaker.FailThreshold = 5
	r.Scheduler.Breaker.OpenDuration = 60 * time.Second
	r.Scheduler.Breaker.OpenMax = 30 * time.Minute
	r.Scheduler.Breaker.Backoff = "exponential"
	r.Scheduler.Retry.MaxAttempts = 3
	r.Scheduler.Retry.BackoffMs = []int{100, 500, 2000}
	r.Scheduler.Sticky.Enabled = true
	r.Scheduler.Sticky.FallbackReplay = "full"
	r.Scheduler.Sticky.StickyTTL = 30 * time.Minute
	r.Scheduler.Failover.HeadBuffer.MaxDuration = 500 * time.Millisecond
	r.Scheduler.Failover.HeadBuffer.MaxBytes = 16 * 1024
	r.Scheduler.Failover.HeadBuffer.MaxEvents = 5
	r.Scheduler.Failover.MidStream.Mode = "best_effort_close"
	r.Scheduler.Failover.MidStream.MarkAccountOnFail = "suspected"
	r.Scheduler.Failover.NeverFail.Enabled = true
	r.Scheduler.Failover.NeverFail.MaxWait = 60 * time.Second
	r.Scheduler.Failover.NeverFail.ProbeInterval = 30 * time.Second
	r.Scheduler.Failover.NeverFail.ProbeCost = "minimal"
	r.Scheduler.Failover.NeverFail.DegradedResponse = "429_retry_after"
	r.Scheduler.Prober.BackgroundInterval = 30 * time.Second
	r.Scheduler.Prober.OnStateChange = "confirmed"
	r.Scheduler.Consistency.RequireSamePlanTier = true
	r.Scheduler.Consistency.RequireSameModels = true
	r.Scheduler.Consistency.FullHistoryReplayOnSwitch = true
	r.Scheduler.Consistency.DisableAccountPersonalization = true
	r.Scheduler.Quota.DrainThreshold = 0.10
	r.Scheduler.Quota.CostPower = 2.0

	r.TokenOptimizer.Mode = "off"
	r.TokenOptimizer.MinToolOutputBytes = 32 << 10
	r.TokenOptimizer.MaxOptimizedToolOutputBytes = 64 << 10
	r.TokenOptimizer.HeadLines = 80
	r.TokenOptimizer.TailLines = 80
	r.TokenOptimizer.ErrorContextLines = 6

	r.Stealth.DefaultTLSProfile = "chrome_124"
	r.Stealth.WarmupOnFirstUse = true
	r.Stealth.MaxRPSPerPersona = 0.5
	r.Stealth.JitterPercent = 20

	r.Discovery.DefaultInterval = 5 * time.Minute
	r.Discovery.StableInterval = 15 * time.Minute
	r.Discovery.UnstableInterval = 1 * time.Minute
	r.Discovery.OnFirstSeen = "immediate"

	r.Cluster.PullInterval = 15 * time.Second
	r.Cluster.PullTimeout = 5 * time.Second

	r.Storage.DBPath = "data/pool.db"
	r.Storage.PasswdPath = "data/passwd.txt"

	r.Logging.Level = "info"
	r.Logging.Format = "json"

	return r
}
