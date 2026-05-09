package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/llm-pool/gateway/internal/domain"
)

const SettingRemoteChatAccountID = "remote_chat.account_id"

// extendMigrate adds the tables introduced after the MVP store: tenants
// master keys, dynamic groups, api keys, quota samples, persona bundles
// remain in store.go's migrate(); these are additive and idempotent.
func (s *Store) extendMigrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS app_settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tenant_secrets (
			tenant_id TEXT PRIMARY KEY,
			master_key_hash TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			key_value TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			group_id TEXT NOT NULL,
			label TEXT,
			created_at INTEGER NOT NULL,
			revoked_at INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_apikeys_tenant ON api_keys(tenant_id)`,
		`CREATE INDEX IF NOT EXISTS idx_apikeys_group ON api_keys(group_id)`,
		`CREATE TABLE IF NOT EXISTS dyn_groups (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			provider TEXT NOT NULL,
			models_json TEXT NOT NULL,
			model_aliases_json TEXT NOT NULL,
			model_whitelist_json TEXT NOT NULL,
			account_ids_json TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_dyn_groups_tenant ON dyn_groups(tenant_id)`,
		`CREATE TABLE IF NOT EXISTS dyn_tenants (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS quota_samples (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id TEXT NOT NULL,
			at INTEGER NOT NULL,
			ewma_ms REAL,
			inflight INTEGER,
			confidence TEXT,
			breaker INTEGER,
			used_short INTEGER,
			limit_short INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_qs_account_at ON quota_samples(account_id, at DESC)`,
		`CREATE TABLE IF NOT EXISTS request_samples (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			at INTEGER NOT NULL,
			tenant_id TEXT,
			group_id TEXT,
			provider TEXT,
			model TEXT,
			account_id TEXT,
			api_key TEXT,
			cache_hit INTEGER DEFAULT 0,
			latency_ms INTEGER,
			input_tokens INTEGER,
			output_tokens INTEGER,
			status TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_rs_at ON request_samples(at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_rs_tenant_at ON request_samples(tenant_id, at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_rs_apikey ON request_samples(api_key)`,
		`CREATE TABLE IF NOT EXISTS conv_records (
			conv_hash TEXT PRIMARY KEY,
			account_id TEXT,
			upstream_conv_id TEXT,
			api_key TEXT,
			tenant_id TEXT,
			group_id TEXT,
			last_msg_index INTEGER,
			hits INTEGER DEFAULT 0,
			misses INTEGER DEFAULT 0,
			created_at INTEGER,
			last_seen INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_conv_apikey ON conv_records(api_key)`,
		`CREATE INDEX IF NOT EXISTS idx_conv_tenant ON conv_records(tenant_id)`,
		`CREATE TABLE IF NOT EXISTS tenant_users (
			username TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tu_tenant ON tenant_users(tenant_id)`,
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, st := range stmts {
		if _, err := tx.Exec(st); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("exec %q: %w", st, err)
		}
	}
	// Idempotent column adds for upgraded databases (ignore "duplicate column").
	for _, alter := range []string{
		`ALTER TABLE request_samples ADD COLUMN api_key TEXT`,
		`ALTER TABLE request_samples ADD COLUMN cache_hit INTEGER DEFAULT 0`,
		`ALTER TABLE dyn_groups ADD COLUMN system_prompt TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE dyn_groups ADD COLUMN system_prompt_mode TEXT NOT NULL DEFAULT 'prepend'`,
		`ALTER TABLE dyn_groups ADD COLUMN system_prompt_injection TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE dyn_groups ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE dyn_groups ADD COLUMN forced_model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE request_samples ADD COLUMN cache_read_tokens INTEGER DEFAULT 0`,
		`ALTER TABLE request_samples ADD COLUMN cache_creation_tokens INTEGER DEFAULT 0`,
		`ALTER TABLE api_keys ADD COLUMN ip_whitelist TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE api_keys ADD COLUMN quota_balance REAL NOT NULL DEFAULT -1`,
		`ALTER TABLE api_keys ADD COLUMN rpm_limit INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := tx.Exec(alter); err != nil {
			// silently ignore - column already exists
			_ = err
		}
	}
	return tx.Commit()
}

// GetSetting returns a small application-level setting.
func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key=?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetSetting upserts a small application-level setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO app_settings(key, value, updated_at) VALUES(?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, time.Now().Unix())
	return err
}

// DeleteSetting removes an application-level setting.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM app_settings WHERE key=?`, key)
	return err
}

// ----- Tenants -----

type Tenant struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

func (s *Store) ListDynTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at FROM dyn_tenants ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		var t Tenant
		var ts int64
		if err := rows.Scan(&t.ID, &t.Name, &ts); err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(ts, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) UpsertDynTenant(ctx context.Context, t Tenant) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO dyn_tenants(id, name, created_at) VALUES(?,?,?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name`,
		t.ID, t.Name, t.CreatedAt.Unix(),
	)
	return err
}

func (s *Store) DeleteDynTenant(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM api_keys WHERE tenant_id=?`,
		`DELETE FROM dyn_groups WHERE tenant_id=?`,
		`DELETE FROM accounts WHERE tenant_id=?`,
		`DELETE FROM tenant_secrets WHERE tenant_id=?`,
		`DELETE FROM dyn_tenants WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// SetTenantMasterKey records a fresh master key for the tenant, returning the
// plaintext (only seen at creation time). The key is hashed (SHA-256) before
// persistence.
func (s *Store) SetTenantMasterKey(ctx context.Context, tenantID string) (string, error) {
	plaintext := "mk-" + randomToken(32)
	h := sha256.Sum256([]byte(plaintext))
	hash := hex.EncodeToString(h[:])
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tenant_secrets(tenant_id, master_key_hash, created_at, updated_at) VALUES(?,?,?,?)
		 ON CONFLICT(tenant_id) DO UPDATE SET master_key_hash=excluded.master_key_hash, updated_at=excluded.updated_at`,
		tenantID, hash, now, now)
	if err != nil {
		return "", err
	}
	return plaintext, nil
}

func (s *Store) VerifyTenantMasterKey(ctx context.Context, key string) (string, bool, error) {
	h := sha256.Sum256([]byte(key))
	hash := hex.EncodeToString(h[:])
	var tenantID string
	err := s.db.QueryRowContext(ctx, `SELECT tenant_id FROM tenant_secrets WHERE master_key_hash=?`, hash).Scan(&tenantID)
	if err != nil {
		return "", false, nil
	}
	return tenantID, true, nil
}

// ----- Dynamic Groups -----

type DynGroup struct {
	ID                    string
	TenantID              string
	Provider              string
	Models                []string
	ModelAliases          map[string]string
	ModelWhitelist        []string
	AccountIDs            []string
	SystemPrompt          string
	SystemPromptMode      string
	SystemPromptInjection string
	ReasoningEffort       string
	ForcedModel           string
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

func (g DynGroup) ToDomain() domain.Group {
	return domain.Group{
		ID:                    g.ID,
		TenantID:              g.TenantID,
		Provider:              g.Provider,
		Models:                g.Models,
		ModelAliases:          g.ModelAliases,
		ModelWhitelist:        g.ModelWhitelist,
		AccountIDs:            g.AccountIDs,
		SystemPrompt:          g.SystemPrompt,
		SystemPromptMode:      g.SystemPromptMode,
		SystemPromptInjection: g.SystemPromptInjection,
		ReasoningEffort:       g.ReasoningEffort,
		ForcedModel:           g.ForcedModel,
	}
}

func (s *Store) ListDynGroups(ctx context.Context, tenantID string) ([]DynGroup, error) {
	q := `SELECT id, tenant_id, provider, models_json, model_aliases_json, model_whitelist_json, account_ids_json, system_prompt, system_prompt_mode, system_prompt_injection, reasoning_effort, forced_model, created_at, updated_at FROM dyn_groups`
	args := []any{}
	if tenantID != "" {
		q += ` WHERE tenant_id=?`
		args = append(args, tenantID)
	}
	q += ` ORDER BY created_at`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DynGroup
	for rows.Next() {
		var g DynGroup
		var modelsJSON, aliasesJSON, whitelistJSON, accountsJSON string
		var ct, ut int64
		if err := rows.Scan(&g.ID, &g.TenantID, &g.Provider, &modelsJSON, &aliasesJSON, &whitelistJSON, &accountsJSON, &g.SystemPrompt, &g.SystemPromptMode, &g.SystemPromptInjection, &g.ReasoningEffort, &g.ForcedModel, &ct, &ut); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(modelsJSON), &g.Models)
		_ = json.Unmarshal([]byte(aliasesJSON), &g.ModelAliases)
		_ = json.Unmarshal([]byte(whitelistJSON), &g.ModelWhitelist)
		_ = json.Unmarshal([]byte(accountsJSON), &g.AccountIDs)
		g.CreatedAt = time.Unix(ct, 0)
		g.UpdatedAt = time.Unix(ut, 0)
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) UpsertDynGroup(ctx context.Context, g DynGroup) error {
	if g.ModelAliases == nil {
		g.ModelAliases = map[string]string{}
	}
	if g.Models == nil {
		g.Models = []string{}
	}
	if g.ModelWhitelist == nil {
		g.ModelWhitelist = []string{}
	}
	if g.AccountIDs == nil {
		g.AccountIDs = []string{}
	}
	if g.SystemPromptInjection == "always" {
		g.SystemPromptInjection = ""
	}
	mb, _ := json.Marshal(g.Models)
	ab, _ := json.Marshal(g.ModelAliases)
	wb, _ := json.Marshal(g.ModelWhitelist)
	ib, _ := json.Marshal(g.AccountIDs)
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO dyn_groups(id, tenant_id, provider, models_json, model_aliases_json, model_whitelist_json, account_ids_json, system_prompt, system_prompt_mode, system_prompt_injection, reasoning_effort, forced_model, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   tenant_id=excluded.tenant_id, provider=excluded.provider,
		   models_json=excluded.models_json, model_aliases_json=excluded.model_aliases_json,
		   model_whitelist_json=excluded.model_whitelist_json, account_ids_json=excluded.account_ids_json,
		   system_prompt=excluded.system_prompt, system_prompt_mode=excluded.system_prompt_mode,
		   system_prompt_injection=excluded.system_prompt_injection,
		   reasoning_effort=excluded.reasoning_effort, forced_model=excluded.forced_model,
		   updated_at=excluded.updated_at`,
		g.ID, g.TenantID, g.Provider, string(mb), string(ab), string(wb), string(ib),
		g.SystemPrompt, g.SystemPromptMode, g.SystemPromptInjection, g.ReasoningEffort, g.ForcedModel, now, now)
	return err
}

func (s *Store) DeleteDynGroup(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM api_keys WHERE group_id=?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM dyn_groups WHERE id=?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ----- API Keys -----

type APIKeyRecord struct {
	Value        string
	TenantID     string
	GroupID      string
	Label        string
	IPWhitelist  string  // comma-separated CIDR list, empty = allow all
	QuotaBalance float64 // -1 = unlimited
	RPMLimit     int     // 0 = use gateway default
	CreatedAt    time.Time
	RevokedAt    *time.Time
}

func (s *Store) ListAPIKeys(ctx context.Context, tenantID, groupID string) ([]APIKeyRecord, error) {
	q := `SELECT key_value, tenant_id, group_id, label, created_at, revoked_at FROM api_keys WHERE 1=1`
	args := []any{}
	if tenantID != "" {
		q += ` AND tenant_id=?`
		args = append(args, tenantID)
	}
	if groupID != "" {
		q += ` AND group_id=?`
		args = append(args, groupID)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKeyRecord
	for rows.Next() {
		var k APIKeyRecord
		var ct int64
		var rv *int64
		if err := rows.Scan(&k.Value, &k.TenantID, &k.GroupID, &k.Label, &ct, &rv); err != nil {
			return nil, err
		}
		k.CreatedAt = time.Unix(ct, 0)
		if rv != nil {
			t := time.Unix(*rv, 0)
			k.RevokedAt = &t
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) CreateAPIKey(ctx context.Context, tenantID, groupID, label string) (APIKeyRecord, error) {
	value := "sk-pool-" + randomToken(28)
	rec := APIKeyRecord{
		Value:     value,
		TenantID:  tenantID,
		GroupID:   groupID,
		Label:     label,
		CreatedAt: time.Now(),
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys(key_value, tenant_id, group_id, label, created_at) VALUES(?,?,?,?,?)`,
		rec.Value, rec.TenantID, rec.GroupID, rec.Label, rec.CreatedAt.Unix())
	return rec, err
}

func (s *Store) RevokeAPIKey(ctx context.Context, value string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET revoked_at=? WHERE key_value=? AND revoked_at IS NULL`,
		time.Now().Unix(), value)
	return err
}

// APIKeyInfo is the resolved metadata for an API key.
type APIKeyInfo struct {
	TenantID     string
	GroupID      string
	IPWhitelist  string
	QuotaBalance float64
	RPMLimit     int
}

// ResolveAPIKey returns the (tenant, group) the key belongs to, or false if
// unknown / revoked.
func (s *Store) ResolveAPIKey(ctx context.Context, value string) (tenantID, groupID string, ok bool, err error) {
	var rv *int64
	err = s.db.QueryRowContext(ctx,
		`SELECT tenant_id, group_id, revoked_at FROM api_keys WHERE key_value=?`, value).
		Scan(&tenantID, &groupID, &rv)
	if err != nil {
		return "", "", false, nil
	}
	if rv != nil {
		return "", "", false, nil
	}
	return tenantID, groupID, true, nil
}

// ResolveAPIKeyFull returns full key metadata including IP whitelist and quota.
func (s *Store) ResolveAPIKeyFull(ctx context.Context, value string) (APIKeyInfo, bool, error) {
	var info APIKeyInfo
	var rv *int64
	err := s.db.QueryRowContext(ctx,
		`SELECT tenant_id, group_id, revoked_at, COALESCE(ip_whitelist,''), COALESCE(quota_balance,-1), COALESCE(rpm_limit,0) FROM api_keys WHERE key_value=?`, value).
		Scan(&info.TenantID, &info.GroupID, &rv, &info.IPWhitelist, &info.QuotaBalance, &info.RPMLimit)
	if err != nil {
		return APIKeyInfo{}, false, nil
	}
	if rv != nil {
		return APIKeyInfo{}, false, nil
	}
	return info, true, nil
}

// DeductQuota subtracts from a key's quota balance. No-op if balance is -1 (unlimited).
func (s *Store) DeductQuota(ctx context.Context, keyValue string, amount float64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET quota_balance = quota_balance - ? WHERE key_value=? AND quota_balance >= 0`,
		amount, keyValue)
	return err
}

// ----- Audit log query -----

type AuditEntry struct {
	ID        int64
	At        time.Time
	Level     string
	Category  string
	AccountID string
	GroupID   string
	Message   string
}

func (s *Store) QueryAudit(ctx context.Context, limit int, sinceID int64) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, at, level, category, account_id, group_id, message
		 FROM audit_log WHERE id > ? ORDER BY id DESC LIMIT ?`, sinceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.Level, &e.Category, &e.AccountID, &e.GroupID, &e.Message); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// TenantRequestLog is one request_samples row for the portal usage view.
type TenantRequestLog struct {
	At           time.Time
	Model        string
	GroupID      string
	Provider     string
	InputTokens  int
	OutputTokens int
	DurationMs   int
	Status       string
}

// QueryAuditByTenant returns recent request_samples rows for a tenant.
func (s *Store) QueryAuditByTenant(ctx context.Context, tenantID string, limit int) ([]TenantRequestLog, error) {
	if limit <= 0 || limit > 2000 {
		limit = 200
	}
	since := time.Now().Add(-7 * 24 * time.Hour).Unix()
	rows, err := s.db.QueryContext(ctx,
		`SELECT at, model, group_id, provider, input_tokens, output_tokens, latency_ms, status
		 FROM request_samples WHERE tenant_id=? AND at>=? ORDER BY at DESC LIMIT ?`,
		tenantID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TenantRequestLog
	for rows.Next() {
		var l TenantRequestLog
		var at int64
		if err := rows.Scan(&at, &l.Model, &l.GroupID, &l.Provider,
			&l.InputTokens, &l.OutputTokens, &l.DurationMs, &l.Status); err != nil {
			continue
		}
		l.At = time.Unix(at, 0)
		out = append(out, l)
	}
	return out, rows.Err()
}

// ----- Quota samples -----

type QuotaSample struct {
	At         time.Time
	EWMAMs     float64
	Inflight   int
	Confidence string
	Breaker    int
	UsedShort  int64
	LimitShort int64
}

func (s *Store) AppendQuotaSample(ctx context.Context, accountID string, sample QuotaSample) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO quota_samples(account_id, at, ewma_ms, inflight, confidence, breaker, used_short, limit_short)
		 VALUES(?,?,?,?,?,?,?,?)`,
		accountID, sample.At.Unix(), sample.EWMAMs, sample.Inflight,
		sample.Confidence, sample.Breaker, sample.UsedShort, sample.LimitShort)
	return err
}

func (s *Store) QueryQuotaSamples(ctx context.Context, accountID string, since time.Time, limit int) ([]QuotaSample, error) {
	if limit <= 0 || limit > 5000 {
		limit = 720
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT at, ewma_ms, inflight, confidence, breaker, used_short, limit_short
		 FROM quota_samples WHERE account_id=? AND at>=? ORDER BY at ASC LIMIT ?`,
		accountID, since.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaSample
	for rows.Next() {
		var s QuotaSample
		var at int64
		if err := rows.Scan(&at, &s.EWMAMs, &s.Inflight, &s.Confidence, &s.Breaker, &s.UsedShort, &s.LimitShort); err != nil {
			return nil, err
		}
		s.At = time.Unix(at, 0)
		out = append(out, s)
	}
	return out, rows.Err()
}

// PruneQuotaSamples keeps only data within `keep` for memory hygiene.
func (s *Store) PruneQuotaSamples(ctx context.Context, keep time.Duration) error {
	cutoff := time.Now().Add(-keep).Unix()
	_, err := s.db.ExecContext(ctx, `DELETE FROM quota_samples WHERE at < ?`, cutoff)
	return err
}

// ----- Request samples (for charts) -----

type RequestSample struct {
	At                  time.Time
	TenantID            string
	GroupID             string
	Provider            string
	Model               string
	AccountID           string
	APIKey              string
	CacheHit            bool
	LatencyMs           int64
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
	Status              string
}

func (s *Store) AppendRequestSample(ctx context.Context, sample RequestSample) error {
	ch := 0
	if sample.CacheHit {
		ch = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO request_samples(at, tenant_id, group_id, provider, model, account_id, api_key, cache_hit, latency_ms, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, status)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sample.At.Unix(), sample.TenantID, sample.GroupID, sample.Provider, sample.Model,
		sample.AccountID, sample.APIKey, ch, sample.LatencyMs, sample.InputTokens, sample.OutputTokens,
		sample.CacheReadTokens, sample.CacheCreationTokens, sample.Status)
	return err
}

type RequestRollup struct {
	Bucket       time.Time
	Count        int
	OkCount      int
	ErrCount     int
	InputTokens  int64
	OutputTokens int64
	AvgLatencyMs float64
}

// QueryRequestSeries buckets request samples into N intervals over `window`.
// Uses SQL aggregation so only `buckets` rows are returned regardless of traffic volume.
func (s *Store) QueryRequestSeries(ctx context.Context, tenantID string, window time.Duration, buckets int) ([]RequestRollup, error) {
	if buckets <= 0 {
		buckets = 60
	}
	now := time.Now()
	from := now.Add(-window)
	bucketSec := int64(window.Seconds()) / int64(buckets)
	if bucketSec < 1 {
		bucketSec = 1
	}
	fromUnix := from.Unix()

	q := `SELECT (at - ?) / ? AS bucket_idx,
	             COUNT(*),
	             COALESCE(SUM(CASE WHEN status='ok' THEN 1 ELSE 0 END), 0),
	             COALESCE(SUM(CASE WHEN status!='ok' THEN 1 ELSE 0 END), 0),
	             COALESCE(SUM(input_tokens), 0),
	             COALESCE(SUM(output_tokens), 0),
	             COALESCE(AVG(latency_ms), 0)
	      FROM request_samples WHERE at >= ?`
	args := []any{fromUnix, bucketSec, fromUnix}
	if tenantID != "" {
		q += ` AND tenant_id=?`
		args = append(args, tenantID)
	}
	q += ` GROUP BY bucket_idx ORDER BY bucket_idx`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]RequestRollup, buckets)
	for i := range out {
		out[i].Bucket = from.Add(time.Duration(int64(i)*bucketSec) * time.Second)
	}
	for rows.Next() {
		var idx int
		var cnt, okCnt, errCnt, inTok, outTok int64
		var avgLat float64
		if err := rows.Scan(&idx, &cnt, &okCnt, &errCnt, &inTok, &outTok, &avgLat); err != nil {
			return nil, err
		}
		if idx < 0 {
			continue
		}
		if idx >= buckets {
			idx = buckets - 1
		}
		out[idx].Count += int(cnt)
		out[idx].OkCount += int(okCnt)
		out[idx].ErrCount += int(errCnt)
		out[idx].InputTokens += inTok
		out[idx].OutputTokens += outTok
		out[idx].AvgLatencyMs = avgLat
	}
	return out, rows.Err()
}

func (s *Store) PruneRequestSamples(ctx context.Context, keep time.Duration) error {
	cutoff := time.Now().Add(-keep).Unix()
	_, err := s.db.ExecContext(ctx, `DELETE FROM request_samples WHERE at < ?`, cutoff)
	return err
}

// QueryDailyTokenUsage returns total input/output tokens since the given time.
func (s *Store) QueryDailyTokenUsage(ctx context.Context, since time.Time) (int64, int64, error) {
	var input, output int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0)
		 FROM request_samples WHERE at >= ? AND status='ok'`, since.Unix()).Scan(&input, &output)
	return input, output, err
}

// CacheStat is the (hit, miss, total) tuple for a cache-hit display.
type CacheStat struct {
	Total       int64
	Hits        int64
	Miss        int64
	TokensSaved int64
}

func (c CacheStat) Ratio() float64 {
	if c.Total == 0 {
		return 0
	}
	return float64(c.Hits) / float64(c.Total)
}

// CacheHitOverall returns hit/miss totals across the whole pool within window.
func (s *Store) CacheHitOverall(ctx context.Context, window time.Duration) (CacheStat, error) {
	from := time.Now().Add(-window).Unix()
	var total, hits, saved int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN cache_hit=1 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(cache_read_tokens), 0)
		 FROM request_samples WHERE at >= ?`, from).Scan(&total, &hits, &saved)
	if err != nil {
		return CacheStat{}, err
	}
	return CacheStat{Total: total, Hits: hits, Miss: total - hits, TokensSaved: saved}, nil
}

// CacheHitByTenant returns hit/miss for a single tenant within window.
func (s *Store) CacheHitByTenant(ctx context.Context, tenantID string, window time.Duration) (CacheStat, error) {
	from := time.Now().Add(-window).Unix()
	var total, hits, saved int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN cache_hit=1 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(cache_read_tokens), 0)
		 FROM request_samples WHERE at >= ? AND tenant_id=?`, from, tenantID).Scan(&total, &hits, &saved)
	if err != nil {
		return CacheStat{}, err
	}
	return CacheStat{Total: total, Hits: hits, Miss: total - hits, TokensSaved: saved}, nil
}

// KeyCacheStat is one row in the per-API-key cache hit display.
type KeyCacheStat struct {
	APIKey     string  `json:"api_key"`
	Label      string  `json:"label"`
	GroupID    string  `json:"group_id"`
	TenantID   string  `json:"tenant_id"`
	Total      int64   `json:"total"`
	Hits       int64   `json:"hits"`
	Miss       int64   `json:"miss"`
	HitRatio   float64 `json:"hit_ratio"`
	LastUsedAt int64   `json:"last_used_at"`
}

// CacheHitByAPIKey returns per-key cache stats, optionally restricted to a tenant.
func (s *Store) CacheHitByAPIKey(ctx context.Context, tenantID string, window time.Duration) ([]KeyCacheStat, error) {
	from := time.Now().Add(-window).Unix()
	q := `SELECT api_key, COUNT(*),
		 COALESCE(SUM(CASE WHEN cache_hit=1 THEN 1 ELSE 0 END),0),
		 MAX(at) FROM request_samples
		 WHERE at >= ? AND api_key != ''`
	args := []any{from}
	if tenantID != "" {
		q += ` AND tenant_id=?`
		args = append(args, tenantID)
	}
	q += ` GROUP BY api_key ORDER BY COUNT(*) DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KeyCacheStat{}
	for rows.Next() {
		var k KeyCacheStat
		if err := rows.Scan(&k.APIKey, &k.Total, &k.Hits, &k.LastUsedAt); err != nil {
			return nil, err
		}
		k.Miss = k.Total - k.Hits
		if k.Total > 0 {
			k.HitRatio = float64(k.Hits) / float64(k.Total)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	// Enrich with label / tenant / group from api_keys table.
	for i := range out {
		var label, gID, tID string
		_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(label,''), tenant_id, group_id FROM api_keys WHERE key_value=?`, out[i].APIKey).
			Scan(&label, &tID, &gID)
		out[i].Label = label
		out[i].TenantID = tID
		out[i].GroupID = gID
	}
	return out, nil
}

// CacheSeriesPoint is a per-bucket hit/total tuple for a sparkline.
type CacheSeriesPoint struct {
	Bucket time.Time `json:"bucket"`
	Total  int       `json:"total"`
	Hits   int       `json:"hits"`
}

// CacheHitSeries buckets requests over `window` into `buckets` slots and reports
// hit/total per bucket. Tenant filter optional. Uses SQL aggregation.
func (s *Store) CacheHitSeries(ctx context.Context, tenantID string, window time.Duration, buckets int) ([]CacheSeriesPoint, error) {
	if buckets <= 0 {
		buckets = 60
	}
	now := time.Now()
	from := now.Add(-window)
	bucketSec := int64(window.Seconds()) / int64(buckets)
	if bucketSec < 1 {
		bucketSec = 1
	}
	fromUnix := from.Unix()

	q := `SELECT (at - ?) / ? AS bucket_idx,
	             COUNT(*),
	             COALESCE(SUM(CASE WHEN cache_hit=1 THEN 1 ELSE 0 END), 0)
	      FROM request_samples WHERE at >= ?`
	args := []any{fromUnix, bucketSec, fromUnix}
	if tenantID != "" {
		q += ` AND tenant_id=?`
		args = append(args, tenantID)
	}
	q += ` GROUP BY bucket_idx ORDER BY bucket_idx`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CacheSeriesPoint, buckets)
	for i := range out {
		out[i].Bucket = from.Add(time.Duration(int64(i)*bucketSec) * time.Second)
	}
	for rows.Next() {
		var idx, total, hits int
		if err := rows.Scan(&idx, &total, &hits); err != nil {
			return nil, err
		}
		if idx < 0 {
			continue
		}
		if idx >= buckets {
			idx = buckets - 1
		}
		out[idx].Total += total
		out[idx].Hits += hits
	}
	return out, rows.Err()
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)[:n]
}

// ----- Tenant users (username + password) -----

type TenantUser struct {
	Username  string
	TenantID  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateTenantUser stores a hashed password for the (username, tenantID) pair.
// Hashing must be done by the caller — store keeps it agnostic to bcrypt etc.
func (s *Store) CreateTenantUser(ctx context.Context, username, tenantID, passwordHash string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tenant_users(username, tenant_id, password_hash, created_at, updated_at) VALUES(?,?,?,?,?)
		 ON CONFLICT(username) DO UPDATE SET password_hash=excluded.password_hash, tenant_id=excluded.tenant_id, updated_at=excluded.updated_at`,
		username, tenantID, passwordHash, now, now)
	return err
}

func (s *Store) DeleteTenantUser(ctx context.Context, username string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tenant_users WHERE username=?`, username)
	return err
}

func (s *Store) ListTenantUsers(ctx context.Context, tenantID string) ([]TenantUser, error) {
	q := `SELECT username, tenant_id, created_at, updated_at FROM tenant_users`
	args := []any{}
	if tenantID != "" {
		q += ` WHERE tenant_id=?`
		args = append(args, tenantID)
	}
	q += ` ORDER BY created_at`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TenantUser
	for rows.Next() {
		var u TenantUser
		var ct, ut int64
		if err := rows.Scan(&u.Username, &u.TenantID, &ct, &ut); err != nil {
			return nil, err
		}
		u.CreatedAt = time.Unix(ct, 0)
		u.UpdatedAt = time.Unix(ut, 0)
		out = append(out, u)
	}
	return out, rows.Err()
}

// LookupTenantUser returns the (tenantID, password_hash) for a username.
func (s *Store) LookupTenantUser(ctx context.Context, username string) (tenantID, passwordHash string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT tenant_id, password_hash FROM tenant_users WHERE username=?`, username).
		Scan(&tenantID, &passwordHash)
	if err != nil {
		return "", "", false, nil
	}
	return tenantID, passwordHash, true, nil
}
