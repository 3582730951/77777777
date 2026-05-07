// Package store handles SQLite persistence and AES-GCM encryption of
// sensitive credential blobs. Hot state lives in-memory; this package only
// owns cold-state durability (accounts, groups, audit, quota history).
package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/llm-pool/gateway/internal/domain"
)

type Store struct {
	db        *sql.DB
	masterKey []byte
}

type Options struct {
	SQLiteMmapBytes int64
	MaxOpenConns    int
	MaxIdleConns    int
}

func Open(dbPath, masterKey string) (*Store, error) {
	return OpenWithOptions(dbPath, masterKey, Options{})
}

func OpenWithOptions(dbPath, masterKey string, opts Options) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", dbPath)
	if opts.SQLiteMmapBytes > 0 {
		dsn += fmt.Sprintf("&_pragma=mmap_size(%d)", opts.SQLiteMmapBytes)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	maxOpen := opts.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = 4
	}
	maxIdle := opts.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = 2
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(30 * time.Minute)

	s := &Store{db: db}
	if masterKey != "" {
		k := sha256.Sum256([]byte(masterKey))
		s.masterKey = k[:]
	}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := s.extendMigrate(); err != nil {
		return nil, fmt.Errorf("extend migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// QueryDB exposes the raw database for ad-hoc admin queries.
func (s *Store) QueryDB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS tenants (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS groups (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			provider TEXT NOT NULL,
			data TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS accounts (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			provider TEXT NOT NULL,
			credential_blob BLOB NOT NULL,
			stealth_profile TEXT,
			ua TEXT,
			proxy TEXT,
			state TEXT NOT NULL,
			plan_tier TEXT,
			data TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_tenant ON accounts(tenant_id)`,
		`CREATE TABLE IF NOT EXISTS quota_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id TEXT NOT NULL,
			at INTEGER NOT NULL,
			used_short INTEGER, limit_short INTEGER,
			used_long INTEGER, limit_long INTEGER,
			event TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_qh_account_at ON quota_history(account_id, at DESC)`,
		`CREATE TABLE IF NOT EXISTS audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			at INTEGER NOT NULL,
			level TEXT,
			category TEXT,
			account_id TEXT,
			group_id TEXT,
			message TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS admin_users (
			username TEXT PRIMARY KEY,
			password_hash TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS persona_bundles (
			account_id TEXT PRIMARY KEY,
			tls_profile TEXT NOT NULL,
			ua TEXT NOT NULL,
			client_hints TEXT NOT NULL,
			cookie_jar BLOB NOT NULL,
			h2_settings TEXT,
			ip_or_proxy TEXT,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS pending_oauth (
			id TEXT PRIMARY KEY,
			provider TEXT NOT NULL,
			state TEXT NOT NULL,
			code_verifier TEXT NOT NULL,
			redirect_uri TEXT NOT NULL,
			tenant_id TEXT NOT NULL,
			note TEXT,
			status TEXT NOT NULL DEFAULT 'pending',
			data TEXT,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pending_oauth_state ON pending_oauth(state)`,
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
	return tx.Commit()
}

func (s *Store) encrypt(plain []byte) ([]byte, error) {
	if len(s.masterKey) == 0 {
		return plain, nil
	}
	block, err := aes.NewCipher(s.masterKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func (s *Store) decrypt(blob []byte) ([]byte, error) {
	if len(s.masterKey) == 0 {
		return blob, nil
	}
	block, err := aes.NewCipher(s.masterKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ct := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}

func (s *Store) UpsertTenant(ctx context.Context, t domain.Tenant) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tenants(id, name, created_at) VALUES(?,?,?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name`,
		t.ID, t.Name, t.CreatedAt.Unix())
	return err
}

func (s *Store) ListTenants(ctx context.Context) ([]domain.Tenant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at FROM tenants`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Tenant
	for rows.Next() {
		var t domain.Tenant
		var ts int64
		if err := rows.Scan(&t.ID, &t.Name, &ts); err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(ts, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

type AccountSecret struct {
	Cookies      []byte `json:"cookies,omitempty"`
	SessionToken string `json:"session_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

func (s *Store) UpsertAccount(ctx context.Context, a *domain.Account, secret AccountSecret) error {
	secBytes, _ := json.Marshal(secret)
	enc, err := s.encrypt(secBytes)
	if err != nil {
		return err
	}
	dataBytes, _ := json.Marshal(a)
	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO accounts(id, tenant_id, provider, credential_blob, stealth_profile, ua, proxy, state, plan_tier, data, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   credential_blob=excluded.credential_blob,
		   stealth_profile=excluded.stealth_profile,
		   ua=excluded.ua, proxy=excluded.proxy,
		   state=excluded.state, plan_tier=excluded.plan_tier,
		   data=excluded.data, updated_at=excluded.updated_at`,
		a.ID, a.TenantID, a.Provider, enc, a.StealthProfile, a.UA, a.Proxy,
		string(a.State), a.PlanTier, string(dataBytes), now, now)
	return err
}

func (s *Store) ListAccounts(ctx context.Context, tenantID string) ([]*domain.Account, error) {
	q := `SELECT data FROM accounts`
	args := []any{}
	if tenantID != "" {
		q += ` WHERE tenant_id=?`
		args = append(args, tenantID)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Account
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		a := &domain.Account{}
		if err := json.Unmarshal([]byte(data), a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetAccountSecret(ctx context.Context, id string) (AccountSecret, error) {
	var blob []byte
	err := s.db.QueryRowContext(ctx, `SELECT credential_blob FROM accounts WHERE id=?`, id).Scan(&blob)
	if err != nil {
		return AccountSecret{}, err
	}
	dec, err := s.decrypt(blob)
	if err != nil {
		return AccountSecret{}, err
	}
	var sec AccountSecret
	if err := json.Unmarshal(dec, &sec); err != nil {
		return AccountSecret{}, err
	}
	return sec, nil
}

func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id=?`, id)
	return err
}

func (s *Store) AppendAudit(ctx context.Context, level, category, accountID, groupID, msg string) {
	_, _ = s.db.ExecContext(ctx,
		`INSERT INTO audit_log(at, level, category, account_id, group_id, message) VALUES(?,?,?,?,?,?)`,
		time.Now().Unix(), level, category, accountID, groupID, msg)
}

// PruneAuditLog removes audit entries older than `keep` and caps the table
// at `maxRows` to prevent unbounded disk usage on a 1C1G VPS.
func (s *Store) PruneAuditLog(ctx context.Context, keep time.Duration, maxRows int64) error {
	cutoff := time.Now().Add(-keep).Unix()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM audit_log WHERE at < ?`, cutoff); err != nil {
		return err
	}
	if maxRows > 0 {
		_, err := s.db.ExecContext(ctx,
			`DELETE FROM audit_log WHERE id NOT IN (SELECT id FROM audit_log ORDER BY id DESC LIMIT ?)`, maxRows)
		return err
	}
	return nil
}

func (s *Store) UpsertAdminUser(ctx context.Context, username, passwordHash string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO admin_users(username, password_hash, created_at, updated_at) VALUES(?,?,?,?)
		 ON CONFLICT(username) DO UPDATE SET password_hash=excluded.password_hash, updated_at=excluded.updated_at`,
		username, passwordHash, now, now)
	return err
}

func (s *Store) GetAdminUser(ctx context.Context, username string) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM admin_users WHERE username=?`, username).Scan(&hash)
	return hash, err
}

func (s *Store) HasAdminUser(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ── Pending OAuth persistence ──

func (s *Store) UpsertPendingOAuth(ctx context.Context, id, provider, state, codeVerifier, redirectURI, tenantID, note, status, data string, createdAt, expiresAt int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pending_oauth(id, provider, state, code_verifier, redirect_uri, tenant_id, note, status, data, created_at, expires_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET status=excluded.status, data=excluded.data`,
		id, provider, state, codeVerifier, redirectURI, tenantID, note, status, data, createdAt, expiresAt)
	return err
}

func (s *Store) FindPendingOAuthByState(ctx context.Context, state string) (id, provider, codeVerifier, redirectURI, tenantID, note, status, data string, createdAt, expiresAt int64, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT id, provider, code_verifier, redirect_uri, tenant_id, note, status, COALESCE(data,''), created_at, expires_at FROM pending_oauth WHERE state=? AND status='pending'`, state).
		Scan(&id, &provider, &codeVerifier, &redirectURI, &tenantID, &note, &status, &data, &createdAt, &expiresAt)
	return
}

func (s *Store) GetPendingOAuth(ctx context.Context, id string) (provider, state, codeVerifier, redirectURI, tenantID, note, status, data string, createdAt, expiresAt int64, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT provider, state, code_verifier, redirect_uri, tenant_id, note, status, COALESCE(data,''), created_at, expires_at FROM pending_oauth WHERE id=?`, id).
		Scan(&provider, &state, &codeVerifier, &redirectURI, &tenantID, &note, &status, &data, &createdAt, &expiresAt)
	return
}

func (s *Store) UpdatePendingOAuthStatus(ctx context.Context, id, status, data string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE pending_oauth SET status=?, data=? WHERE id=?`, status, data, id)
	return err
}

func (s *Store) GCPendingOAuth(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pending_oauth WHERE expires_at < ?`, time.Now().Add(-1*time.Hour).Unix())
	return err
}
