// Package provider defines the upstream provider interface that adapters
// (chatgpt, claude, gemini) implement. A provider takes an IR.Request plus
// account context and produces a stream of IR events (or a single error if
// the call cannot be initiated). The scheduler is responsible for choosing
// which account to invoke; provider only renders and parses.
package provider

import (
	"context"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
)

type Provider interface {
	Name() string

	// Invoke calls the upstream service for the given account. It returns
	// a channel from which the caller drains IR events. The channel must
	// be closed by the provider after the final EvDone or EvError. Errors
	// before the first event must be reported via err return.
	Invoke(ctx context.Context, account *domain.Account, req *ir.Request) (<-chan ir.Event, error)

	// Probe sends a minimal request to verify account health without
	// substantive token consumption. Returns nil on success.
	Probe(ctx context.Context, account *domain.Account) error

	// Discover queries the upstream's quota / model-list endpoint to
	// refresh the account's QuotaState.DiscoveredModels and short/long
	// window usage. Should be cheap (one or two HTTP calls).
	Discover(ctx context.Context, account *domain.Account) (*domain.QuotaState, error)
}

type Registry struct {
	providers map[string]Provider
}

func NewRegistry() *Registry { return &Registry{providers: make(map[string]Provider)} }

func (r *Registry) Register(p Provider) { r.providers[p.Name()] = p }

func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}
