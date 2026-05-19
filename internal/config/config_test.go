package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultsSupportLongContextAndFiftyConcurrent(t *testing.T) {
	cfg := defaults()
	if cfg.Server.MaxRequestBytes < 512<<20 {
		t.Fatalf("max_request_bytes should allow long context payloads, got %d", cfg.Server.MaxRequestBytes)
	}
	if cfg.Server.ReadTimeout < 300*time.Second {
		t.Fatalf("read timeout should allow slow long-context uploads, got %s", cfg.Server.ReadTimeout)
	}
	if cfg.Server.RequestBodyMemoryBudget < cfg.Server.MaxRequestBytes {
		t.Fatalf("request body memory budget should allow one max-size long context while queuing other large uploads, got %d", cfg.Server.RequestBodyMemoryBudget)
	}
	if cfg.Server.NetworkIngressBytesPerSec <= 0 || cfg.Server.NetworkEgressBytesPerSec <= 0 || cfg.Server.NetworkBurstBytes <= 0 {
		t.Fatalf("network shaping defaults must apply backpressure instead of rejecting downstream clients")
	}
	if cfg.Server.RateLimitBurst < 50 {
		t.Fatalf("rate_limit_burst must allow 50 concurrent starts, got %d", cfg.Server.RateLimitBurst)
	}
	if cfg.Resource.ResponsesStateTTL < 12*time.Hour {
		t.Fatalf("responses state ttl too low for long CLI conversations: %s", cfg.Resource.ResponsesStateTTL)
	}
	if cfg.Resource.ResponsesStateBytes < 256<<20 {
		t.Fatalf("responses state bytes should preserve long conversations while bounding total memory, got %d", cfg.Resource.ResponsesStateBytes)
	}
	if cfg.Resource.GoMemLimit != "768MiB" {
		t.Fatalf("go_mem_limit should leave headroom for a large in-flight context on 1GiB VPS, got %s", cfg.Resource.GoMemLimit)
	}
	if cfg.Scheduler.Quota.DrainThreshold <= 0 || cfg.Scheduler.Quota.DrainThreshold >= 1 {
		t.Fatalf("quota drain threshold should keep low-quota accounts for sticky work only, got %f", cfg.Scheduler.Quota.DrainThreshold)
	}
	if cfg.TokenOptimizer.Mode != "off" || cfg.TokenOptimizer.MinToolOutputBytes <= 0 {
		t.Fatalf("token optimizer defaults should preserve direct Codex request bodies: %+v", cfg.TokenOptimizer)
	}
	if !cfg.Stealth.IdentityRewrite || cfg.Stealth.IdentityPath == "" {
		t.Fatalf("identity rewrite must default on with a persistent identity path: %+v", cfg.Stealth)
	}
}

func TestConfigFilesDoNotDowngradeGPT5Aliases(t *testing.T) {
	paths := []string{
		filepath.Join("..", "..", "config", "config.yaml"),
		filepath.Join("..", "..", "config", "config.example.yaml"),
		filepath.Join("..", "..", "deploy", "config", "config.yaml"),
	}
	for _, path := range paths {
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("load %s: %v", path, err)
		}
		for _, group := range cfg.Groups {
			for from, to := range group.ModelAliases {
				if isGPT5Downgrade(from, to) {
					t.Fatalf("%s group %s downgrades model alias %s -> %s", path, group.ID, from, to)
				}
			}
		}
	}
}

func isGPT5Downgrade(from, to string) bool {
	fromRank, fromOK := gpt5Rank(from)
	toRank, toOK := gpt5Rank(to)
	return fromOK && toOK && toRank < fromRank
}

func gpt5Rank(model string) (int, bool) {
	model = strings.ToLower(model)
	switch {
	case strings.HasPrefix(model, "gpt-5.5"):
		return 550, true
	case strings.HasPrefix(model, "gpt-5.4-mini"):
		return 535, true
	case strings.HasPrefix(model, "gpt-5.4"):
		return 540, true
	case strings.HasPrefix(model, "gpt-5.3"):
		return 530, true
	case strings.HasPrefix(model, "gpt-5.2"):
		return 520, true
	case strings.HasPrefix(model, "gpt-5"):
		return 500, true
	default:
		return 0, false
	}
}
