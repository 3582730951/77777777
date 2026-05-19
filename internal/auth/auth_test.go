package auth

import (
	"testing"

	"github.com/llm-pool/gateway/internal/config"
)

func newTestResolver() *Resolver {
	r := NewResolver()
	r.LoadFromConfig(&config.Root{
		Tenants: []config.Tenant{
			{ID: "t1", Name: "Tenant One"},
		},
		Groups: []config.Group{
			{
				ID:       "g1",
				TenantID: "t1",
				Provider: "claude",
				APIKeys:  []string{"sk-test-123", "sk-test-456"},
				Models:   []string{"claude-3"},
				ModelAliases: map[string]string{
					"fast": "claude-3-haiku",
				},
				APIKeyOverrides: map[string]config.APIKeyOverride{
					"sk-test-456": {ModelAliases: map[string]string{"fast": "claude-3-sonnet"}},
				},
			},
		},
	})
	return r
}

func TestResolve_BearerHeader(t *testing.T) {
	r := newTestResolver()
	res, ok := r.Resolve("Bearer sk-test-123", "", "")
	if !ok {
		t.Fatal("should resolve Bearer key")
	}
	if res.Tenant.ID != "t1" {
		t.Errorf("expected tenant t1, got %s", res.Tenant.ID)
	}
	if res.Group.ID != "g1" {
		t.Errorf("expected group g1, got %s", res.Group.ID)
	}
	if res.APIKey != "sk-test-123" {
		t.Errorf("expected sk-test-123, got %s", res.APIKey)
	}
}

func TestResolve_BearerLowercase(t *testing.T) {
	r := newTestResolver()
	_, ok := r.Resolve("bearer sk-test-123", "", "")
	if !ok {
		t.Error("should handle lowercase bearer")
	}
}

func TestResolve_XAPIKey(t *testing.T) {
	r := newTestResolver()
	_, ok := r.Resolve("", "sk-test-123", "")
	if !ok {
		t.Error("should resolve x-api-key")
	}
}

func TestResolve_XGoogleKey(t *testing.T) {
	r := newTestResolver()
	_, ok := r.Resolve("", "", "sk-test-123")
	if !ok {
		t.Error("should resolve x-goog-api-key")
	}
}

func TestResolve_Priority(t *testing.T) {
	r := newTestResolver()
	res, ok := r.Resolve("Bearer sk-test-123", "sk-test-456", "")
	if !ok {
		t.Fatal("should resolve")
	}
	if res.APIKey != "sk-test-456" {
		t.Error("x-api-key should take priority over Authorization")
	}
}

func TestResolve_Unknown(t *testing.T) {
	r := newTestResolver()
	_, ok := r.Resolve("Bearer unknown-key", "", "")
	if ok {
		t.Error("unknown key should not resolve")
	}
}

func TestResolve_Empty(t *testing.T) {
	r := newTestResolver()
	_, ok := r.Resolve("", "", "")
	if ok {
		t.Error("empty should not resolve")
	}
}

func TestResolve_WithSpaces(t *testing.T) {
	r := newTestResolver()
	_, ok := r.Resolve("  Bearer   sk-test-123  ", "", "")
	if !ok {
		t.Error("should handle extra spaces")
	}
}

func TestResolve_APIKeyOverride(t *testing.T) {
	r := newTestResolver()
	res, _ := r.Resolve("", "sk-test-456", "")
	if res.APIKeyOverride.ModelAliases["fast"] != "claude-3-sonnet" {
		t.Error("API key override should be applied")
	}
}

func TestLoadFromConfig_AutoCreateTenant(t *testing.T) {
	r := NewResolver()
	r.LoadFromConfig(&config.Root{
		Groups: []config.Group{
			{ID: "g1", TenantID: "auto-tenant", APIKeys: []string{"k1"}},
		},
	})
	res, ok := r.Resolve("", "k1", "")
	if !ok {
		t.Fatal("should resolve")
	}
	if res.Tenant.ID != "auto-tenant" {
		t.Error("tenant should be auto-created")
	}
}

func TestLoadFromConfig_Reload(t *testing.T) {
	r := newTestResolver()
	r.LoadFromConfig(&config.Root{
		Groups: []config.Group{
			{ID: "g2", TenantID: "t2", APIKeys: []string{"new-key"}},
		},
	})
	_, ok := r.Resolve("", "sk-test-123", "")
	if ok {
		t.Error("old key should not resolve after reload")
	}
	_, ok = r.Resolve("", "new-key", "")
	if !ok {
		t.Error("new key should resolve after reload")
	}
}

func TestLoadFromConfig_GroupRuntimeControls(t *testing.T) {
	r := NewResolver()
	r.LoadFromConfig(&config.Root{
		Groups: []config.Group{{
			ID:              "g-runtime",
			TenantID:        "t1",
			Provider:        "chatgpt",
			APIKeys:         []string{"sk-runtime"},
			ReasoningEffort: "high",
			ForcedModel:     "gpt-5.2",
		}},
	})
	res, ok := r.Resolve("", "sk-runtime", "")
	if !ok {
		t.Fatal("should resolve runtime control group")
	}
	if res.Group.ReasoningEffort != "high" {
		t.Fatalf("reasoning effort not propagated: %q", res.Group.ReasoningEffort)
	}
	if res.Group.ForcedModel != "gpt-5.2" {
		t.Fatalf("forced model not propagated: %q", res.Group.ForcedModel)
	}
}
