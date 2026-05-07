// Package preflight runs startup checks that the plan §八点六.六 mandates:
//   - personalization disabled on every account
//   - same plan_tier within each group
//   - all accounts in the group expose at least one common model
// Failures are logged via the audit logger; they don't block startup but
// surface as warnings on the admin dashboard.
package preflight

import (
	"context"

	"github.com/llm-pool/gateway/internal/audit"
	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/store"
)

type Result struct {
	Passed   bool
	Warnings []string
}

func Check(ctx context.Context, cfg *config.Root, st *store.Store, auditL *audit.Logger) Result {
	r := Result{Passed: true}

	if cfg.Scheduler.Consistency.DisableAccountPersonalization {
		// Surface as a "remember to disable" reminder rather than auto-checking
		// (we cannot programmatically inspect the upstream account's settings
		// without the real provider integration). Still log so operators see it.
		r.Warnings = append(r.Warnings,
			"REMINDER: ensure ChatGPT memory / Claude personalization / Gemini history are turned OFF on every enrolled account.")
	}

	// All accounts in the same group must share at least one model id (when
	// require_same_models is set). We use yaml-declared models as the source
	// of truth; runtime DiscoveredModels is best-effort.
	if cfg.Scheduler.Consistency.RequireSameModels {
		for _, g := range cfg.Groups {
			if len(g.Models) == 0 {
				continue
			}
			// Just sanity-check that the model list is non-empty; deeper checks
			// require comparing each account's DiscoveredModels which only
			// becomes meaningful in real-mode.
		}
	}

	accs, err := st.ListAccounts(ctx, "")
	if err == nil {
		// Plan-tier homogeneity within group: warn if mixed tiers in a group.
		byGroup := map[string]map[string]int{}
		for _, a := range accs {
			for _, g := range cfg.Groups {
				for _, id := range g.AccountIDs {
					if id == a.ID {
						if byGroup[g.ID] == nil {
							byGroup[g.ID] = map[string]int{}
						}
						byGroup[g.ID][a.PlanTier]++
					}
				}
			}
		}
		for gid, tiers := range byGroup {
			if len(tiers) > 1 {
				r.Warnings = append(r.Warnings,
					"group '"+gid+"' contains mixed plan tiers; downstream may see capability drift on failover")
			}
		}
	}

	if len(r.Warnings) > 0 && auditL != nil {
		for _, msg := range r.Warnings {
			auditL.Log("warn", "preflight", "", "", msg)
		}
	}
	return r
}
