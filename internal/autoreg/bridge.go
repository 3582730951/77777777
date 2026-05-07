package autoreg

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/provider"
	"github.com/llm-pool/gateway/internal/scheduler"
	"github.com/llm-pool/gateway/internal/store"
)

type Bridge struct {
	client      *Client
	store       *store.Store
	syncedStore *SyncedStore
	cfg         Config
	registry    *provider.Registry
	sched       *scheduler.Scheduler
	logger      *slog.Logger
}

func NewBridge(cfg Config, client *Client, st *store.Store, syncedStore *SyncedStore, reg *provider.Registry, sched *scheduler.Scheduler, logger *slog.Logger) *Bridge {
	return &Bridge{
		client:      client,
		store:       st,
		syncedStore: syncedStore,
		cfg:         cfg,
		registry:    reg,
		sched:       sched,
		logger:      logger,
	}
}

func (b *Bridge) Run(ctx context.Context) {
	interval := b.cfg.SyncInterval
	if interval == 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Initial sync after short delay
	time.Sleep(5 * time.Second)
	if err := b.syncOnce(ctx); err != nil {
		b.logger.Warn("autoreg initial sync", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := b.syncOnce(ctx); err != nil {
				b.logger.Warn("autoreg sync", "err", err)
			}
		}
	}
}

func (b *Bridge) syncOnce(ctx context.Context) error {
	page := 1
	synced := 0
	for {
		resp, err := b.client.ListUnsyncedAccounts(ctx, page, 50)
		if err != nil {
			return fmt.Errorf("list unsynced page %d: %w", page, err)
		}
		if len(resp.Items) == 0 {
			break
		}

		for _, pyAcc := range resp.Items {
			if b.syncedStore != nil && b.syncedStore.IsSynced(pyAcc.ID) {
				continue
			}
			if err := b.syncAccount(ctx, pyAcc); err != nil {
				b.logger.Warn("sync account", "py_id", pyAcc.ID, "platform", pyAcc.Platform, "err", err)
				continue
			}
			accID := fmt.Sprintf("autoreg-%s-%d", pyAcc.Platform, pyAcc.ID)
			if b.syncedStore != nil {
				_ = b.syncedStore.MarkSynced(pyAcc.ID, pyAcc.Platform, accID)
			}
			if err := b.client.MarkSynced(ctx, pyAcc.ID); err != nil {
				b.logger.Warn("mark synced", "py_id", pyAcc.ID, "err", err)
			}
			synced++
		}

		if len(resp.Items) < 50 {
			break
		}
		page++
	}
	if synced > 0 {
		b.logger.Info("autoreg synced accounts", "count", synced)
	}
	return nil
}

func (b *Bridge) syncAccount(ctx context.Context, pyAcc PyAccount) error {
	providerName := pyAcc.Platform
	groupID := b.cfg.PlatformGroupMapping[pyAcc.Platform]
	if groupID == "" {
		groupID = pyAcc.Platform
	}

	accID := fmt.Sprintf("autoreg-%s-%d", pyAcc.Platform, pyAcc.ID)
	secret := store.AccountSecret{
		SessionToken: pyAcc.PrimaryToken,
		RefreshToken: extractRefreshToken(pyAcc.Credentials),
	}

	acc := &domain.Account{
		ID:        accID,
		TenantID:  "default",
		Provider:  providerName,
		State:     domain.StateActive,
		PlanTier:  pyAcc.PlanName,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	if err := b.store.UpsertAccount(ctx, acc, secret); err != nil {
		return fmt.Errorf("upsert account: %w", err)
	}

	b.sched.Register(acc)

	// Add to group's AccountIDs (runtime only, config file not changed)
	_ = groupID

	if b.cfg.AutoDiscovery {
		go func() {
			dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			prov, ok := b.registry.Get(providerName)
			if !ok {
				return
			}
			state, err := prov.Discover(dctx, acc)
			if err != nil {
				b.logger.Debug("autoreg discover", "account", accID, "err", err)
				return
			}
			b.sched.UpdateQuota(accID, state)
		}()
	}

	return nil
}

func extractRefreshToken(creds []PyCredential) string {
	for _, c := range creds {
		if c.Key == "refresh_token" || c.Key == "auth_token" || c.Key == "refreshToken" {
			return c.Value
		}
	}
	return ""
}
