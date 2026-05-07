// Package auth resolves an inbound API key to a Tenant + Group + apikey-level
// overrides (model alias). Lookup is in-memory; the gateway rebuilds the table
// when the config or accounts change.
package auth

import (
	"strings"
	"sync"

	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/domain"
)

type Resolved struct {
	Tenant         *domain.Tenant
	Group          *domain.Group
	Federation     *domain.Federation
	APIKey         string
	APIKeyOverride domain.APIKeyOverride
}

type Resolver struct {
	mu      sync.RWMutex
	byKey   map[string]Resolved
	tenants map[string]*domain.Tenant
	groups  map[string]*domain.Group
}

func NewResolver() *Resolver {
	return &Resolver{
		byKey:   make(map[string]Resolved),
		tenants: make(map[string]*domain.Tenant),
		groups:  make(map[string]*domain.Group),
	}
}

// LoadFromConfig (re)builds the lookup table from yaml. Tenants that have no
// matching tenant entry are auto-created.
func (r *Resolver) LoadFromConfig(cfg *config.Root) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byKey = make(map[string]Resolved)
	r.tenants = make(map[string]*domain.Tenant)
	r.groups = make(map[string]*domain.Group)
	for _, t := range cfg.Tenants {
		r.tenants[t.ID] = &domain.Tenant{ID: t.ID, Name: t.Name}
	}
	for _, g := range cfg.Groups {
		tenant, ok := r.tenants[g.TenantID]
		if !ok {
			tenant = &domain.Tenant{ID: g.TenantID, Name: g.TenantID}
			r.tenants[g.TenantID] = tenant
		}
		grp := &domain.Group{
			ID:               g.ID,
			TenantID:         g.TenantID,
			Provider:         g.Provider,
			APIKeys:          append([]string{}, g.APIKeys...),
			Models:           append([]string{}, g.Models...),
			AccountIDs:       append([]string{}, g.AccountIDs...),
			ModelAliases:     cloneStrMap(g.ModelAliases),
			ModelWhitelist:   append([]string{}, g.ModelWhitelist...),
			APIKeyOverrides:  convertOverrides(g.APIKeyOverrides),
			SystemPrompt:     g.SystemPrompt,
			SystemPromptMode: g.SystemPromptMode,
			SourcePlatform:   g.SourcePlatform,
			AutoRegister:     g.AutoRegister,
		}
		r.groups[g.ID] = grp
		for _, k := range g.APIKeys {
			r.byKey[k] = Resolved{
				Tenant:         tenant,
				Group:          grp,
				APIKey:         k,
				APIKeyOverride: grp.APIKeyOverrides[k],
			}
		}
	}
	for _, f := range cfg.Federations {
		fed := &domain.Federation{
			ID:           f.ID,
			Description:  f.Description,
			MemberGroups: append([]string{}, f.MemberGroups...),
			APIKeys:      append([]string{}, f.APIKeys...),
		}
		var tenantID string
		for _, gid := range f.MemberGroups {
			if g, ok := r.groups[gid]; ok && tenantID == "" {
				tenantID = g.TenantID
			}
		}
		fed.TenantID = tenantID
		tenant, ok := r.tenants[tenantID]
		if !ok {
			tenant = &domain.Tenant{ID: tenantID, Name: tenantID}
		}
		for _, k := range f.APIKeys {
			r.byKey[k] = Resolved{
				Tenant:     tenant,
				Federation: fed,
				APIKey:     k,
			}
		}
	}
}

func (r *Resolver) GroupByID(id string) (*domain.Group, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g, ok := r.groups[id]
	return g, ok
}

func cloneStrMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func convertOverrides(in map[string]config.APIKeyOverride) map[string]domain.APIKeyOverride {
	out := make(map[string]domain.APIKeyOverride, len(in))
	for k, v := range in {
		out[k] = domain.APIKeyOverride{ModelAliases: cloneStrMap(v.ModelAliases)}
	}
	return out
}

// Resolve extracts the key from common header forms and looks it up.
// Returns ok=false if the key is unknown.
func (r *Resolver) Resolve(authzHeader, xAPIKey, xGoogleKey string) (Resolved, bool) {
	candidate := ""
	if xAPIKey != "" {
		candidate = strings.TrimSpace(xAPIKey)
	} else if xGoogleKey != "" {
		candidate = strings.TrimSpace(xGoogleKey)
	} else if authzHeader != "" {
		s := strings.TrimSpace(authzHeader)
		s = strings.TrimPrefix(s, "Bearer ")
		s = strings.TrimPrefix(s, "bearer ")
		candidate = strings.TrimSpace(s)
	}
	if candidate == "" {
		return Resolved{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	res, ok := r.byKey[candidate]
	return res, ok
}
