package auth

import (
	"context"
	"sync"
	"time"

	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/store"
)

// DynamicSource is what allows the Resolver to learn keys/groups beyond
// what's hard-coded in YAML. The admin CRUD endpoints write into the store;
// the Resolver pulls fresh state on Reload().
type DynamicSource interface {
	ListDynTenants(ctx context.Context) ([]store.Tenant, error)
	ListDynGroups(ctx context.Context, tenantID string) ([]store.DynGroup, error)
	ListAPIKeys(ctx context.Context, tenantID, groupID string) ([]store.APIKeyRecord, error)
}

// DynamicResolver is the production-grade Resolver that combines compile-time
// YAML defaults with runtime DB state so admins can add accounts / groups /
// keys via the UI without restarting.
type DynamicResolver struct {
	*Resolver
	src     DynamicSource
	mu      sync.RWMutex
	lastErr error
	loaded  time.Time
}

func NewDynamicResolver(src DynamicSource) *DynamicResolver {
	return &DynamicResolver{Resolver: NewResolver(), src: src}
}

func (r *DynamicResolver) Reload(ctx context.Context, cfg *config.Root) error {
	tenants, err := r.src.ListDynTenants(ctx)
	if err != nil {
		r.mu.Lock()
		r.lastErr = err
		r.mu.Unlock()
		return err
	}
	groups, err := r.src.ListDynGroups(ctx, "")
	if err != nil {
		return err
	}
	keys, err := r.src.ListAPIKeys(ctx, "", "")
	if err != nil {
		return err
	}

	combined := mergeWithYAML(cfg, tenants, groups, keys)
	r.Resolver.LoadFromConfig(combined)

	r.mu.Lock()
	r.loaded = time.Now()
	r.lastErr = nil
	r.mu.Unlock()
	return nil
}

func mergeWithYAML(cfg *config.Root, tenants []store.Tenant, groups []store.DynGroup, keys []store.APIKeyRecord) *config.Root {
	out := *cfg
	out.Tenants = append([]config.Tenant{}, cfg.Tenants...)
	out.Groups = append([]config.Group{}, cfg.Groups...)

	seenTenant := make(map[string]bool)
	for _, t := range out.Tenants {
		seenTenant[t.ID] = true
	}
	for _, t := range tenants {
		if !seenTenant[t.ID] {
			out.Tenants = append(out.Tenants, config.Tenant{ID: t.ID, Name: t.Name})
			seenTenant[t.ID] = true
		}
	}

	keysByGroup := map[string][]string{}
	for _, k := range keys {
		if k.RevokedAt != nil {
			continue
		}
		keysByGroup[k.GroupID] = append(keysByGroup[k.GroupID], k.Value)
	}

	seenGroup := make(map[string]int)
	for i, g := range out.Groups {
		seenGroup[g.ID] = i
		if extra, ok := keysByGroup[g.ID]; ok {
			out.Groups[i].APIKeys = append(out.Groups[i].APIKeys, extra...)
		}
	}
	for _, g := range groups {
		if idx, ok := seenGroup[g.ID]; ok {
			out.Groups[idx].Provider = g.Provider
			if g.Models != nil {
				out.Groups[idx].Models = g.Models
			}
			if g.ModelAliases != nil {
				out.Groups[idx].ModelAliases = g.ModelAliases
			}
			if g.ModelWhitelist != nil {
				out.Groups[idx].ModelWhitelist = g.ModelWhitelist
			}
			out.Groups[idx].AccountIDs = g.AccountIDs
			if g.SystemPrompt != "" {
				out.Groups[idx].SystemPrompt = g.SystemPrompt
				out.Groups[idx].SystemPromptMode = g.SystemPromptMode
			}
			continue
		}
		out.Groups = append(out.Groups, config.Group{
			ID:              g.ID,
			TenantID:        g.TenantID,
			Provider:        g.Provider,
			Models:          g.Models,
			ModelAliases:    g.ModelAliases,
			ModelWhitelist:  g.ModelWhitelist,
			AccountIDs:      g.AccountIDs,
			APIKeys:         keysByGroup[g.ID],
			SystemPrompt:    g.SystemPrompt,
			SystemPromptMode: g.SystemPromptMode,
		})
	}
	return &out
}

// Tenants exposes the merged tenant list (yaml + dynamic) for UI consumption.
func (r *DynamicResolver) Tenants() []*domain.Tenant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []*domain.Tenant{}
	for _, t := range r.Resolver.tenants {
		out = append(out, t)
	}
	return out
}

// Groups exposes merged groups.
func (r *DynamicResolver) Groups() []*domain.Group {
	r.Resolver.mu.RLock()
	defer r.Resolver.mu.RUnlock()
	out := []*domain.Group{}
	seen := make(map[string]bool)
	for _, res := range r.Resolver.byKey {
		if res.Group != nil && !seen[res.Group.ID] {
			out = append(out, res.Group)
			seen[res.Group.ID] = true
		}
	}
	return out
}

func (r *DynamicResolver) LastReload() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.loaded
}
