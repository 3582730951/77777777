package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/llm-pool/gateway/internal/domain"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"), "test-key")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpen_CreatesDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "test.db")
	s, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := os.Stat(path); err != nil {
		t.Error("db file should exist")
	}
}

func TestTenant_CRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	err := s.UpsertTenant(ctx, domain.Tenant{ID: "t1", Name: "Test", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	tenants, err := s.ListTenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tenants) != 1 || tenants[0].ID != "t1" {
		t.Error("expected one tenant t1")
	}

	err = s.UpsertTenant(ctx, domain.Tenant{ID: "t1", Name: "Updated", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	tenants, _ = s.ListTenants(ctx)
	if tenants[0].Name != "Updated" {
		t.Error("upsert should update name")
	}
}

func TestAccount_CRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	acct := &domain.Account{
		ID:       "a1",
		TenantID: "t1",
		Provider: "claude",
		State:    domain.StateActive,
	}
	secret := AccountSecret{SessionToken: "tok123"}
	if err := s.UpsertAccount(ctx, acct, secret); err != nil {
		t.Fatal(err)
	}

	accounts, err := s.ListAccounts(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].ID != "a1" {
		t.Error("expected one account a1")
	}

	sec, err := s.GetAccountSecret(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if sec.SessionToken != "tok123" {
		t.Errorf("expected tok123, got %s", sec.SessionToken)
	}

	if err := s.DeleteAccount(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	accounts, _ = s.ListAccounts(ctx, "t1")
	if len(accounts) != 0 {
		t.Error("account should be deleted")
	}
}

func TestAccount_EncryptDecrypt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	acct := &domain.Account{ID: "enc1", TenantID: "t1", Provider: "claude", State: domain.StateActive}
	secret := AccountSecret{RefreshToken: "refresh-secret"}
	s.UpsertAccount(ctx, acct, secret)

	sec, err := s.GetAccountSecret(ctx, "enc1")
	if err != nil {
		t.Fatal(err)
	}
	if sec.RefreshToken != "refresh-secret" {
		t.Error("encrypted secret should round-trip")
	}
}

func TestAccount_NoEncryption(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(filepath.Join(dir, "test.db"), "")
	defer s.Close()
	ctx := context.Background()

	acct := &domain.Account{ID: "plain1", TenantID: "t1", Provider: "claude", State: domain.StateActive}
	secret := AccountSecret{SessionToken: "plain-tok"}
	s.UpsertAccount(ctx, acct, secret)

	sec, _ := s.GetAccountSecret(ctx, "plain1")
	if sec.SessionToken != "plain-tok" {
		t.Error("unencrypted secret should round-trip")
	}
}

func TestAdminUser(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	has, _ := s.HasAdminUser(ctx)
	if has {
		t.Error("should have no admin users initially")
	}

	s.UpsertAdminUser(ctx, "admin", "hash123")
	has, _ = s.HasAdminUser(ctx)
	if !has {
		t.Error("should have admin user")
	}

	hash, err := s.GetAdminUser(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if hash != "hash123" {
		t.Errorf("expected hash123, got %s", hash)
	}
}

func TestAuditLog(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	s.AppendAudit(ctx, "info", "test", "a1", "g1", "test message")
	entries, err := s.QueryAudit(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Message != "test message" {
		t.Error("message mismatch")
	}
}

func TestDynTenants(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	s.UpsertDynTenant(ctx, Tenant{ID: "dt1", Name: "Dyn", CreatedAt: time.Now()})
	tenants, _ := s.ListDynTenants(ctx)
	if len(tenants) != 1 || tenants[0].ID != "dt1" {
		t.Error("expected dyn tenant")
	}

	s.DeleteDynTenant(ctx, "dt1")
	tenants, _ = s.ListDynTenants(ctx)
	if len(tenants) != 0 {
		t.Error("dyn tenant should be deleted")
	}
}

func TestDynGroups(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	g := DynGroup{
		ID:           "dg1",
		TenantID:     "t1",
		Provider:     "claude",
		Models:       []string{"claude-3"},
		ModelAliases: map[string]string{"fast": "haiku"},
		AccountIDs:   []string{"a1"},
	}
	if err := s.UpsertDynGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	groups, _ := s.ListDynGroups(ctx, "t1")
	if len(groups) != 1 || groups[0].ID != "dg1" {
		t.Error("expected dyn group")
	}
	if groups[0].ModelAliases["fast"] != "haiku" {
		t.Error("model aliases mismatch")
	}

	s.DeleteDynGroup(ctx, "dg1")
	groups, _ = s.ListDynGroups(ctx, "t1")
	if len(groups) != 0 {
		t.Error("dyn group should be deleted")
	}
}

func TestAPIKeys(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	rec, err := s.CreateAPIKey(ctx, "t1", "g1", "test key")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Value == "" {
		t.Error("API key should have a value")
	}

	keys, _ := s.ListAPIKeys(ctx, "t1", "")
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}

	tid, gid, ok, _ := s.ResolveAPIKey(ctx, rec.Value)
	if !ok || tid != "t1" || gid != "g1" {
		t.Error("should resolve API key")
	}

	s.RevokeAPIKey(ctx, rec.Value)
	_, _, ok, _ = s.ResolveAPIKey(ctx, rec.Value)
	if ok {
		t.Error("revoked key should not resolve")
	}
}

func TestRequestSamples(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	sample := RequestSample{
		At:          time.Now(),
		TenantID:    "t1",
		Provider:    "claude",
		Model:       "claude-3",
		AccountID:   "a1",
		CacheHit:    true,
		LatencyMs:   200,
		InputTokens: 100,
		OutputTokens: 50,
		Status:      "ok",
	}
	if err := s.AppendRequestSample(ctx, sample); err != nil {
		t.Fatal(err)
	}

	stat, _ := s.CacheHitOverall(ctx, time.Hour)
	if stat.Total != 1 || stat.Hits != 1 {
		t.Errorf("expected 1 total, 1 hit, got %d/%d", stat.Total, stat.Hits)
	}
}

func TestTenantMasterKey(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	key, err := s.SetTenantMasterKey(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if key == "" {
		t.Error("should return plaintext key")
	}

	tid, ok, _ := s.VerifyTenantMasterKey(ctx, key)
	if !ok || tid != "t1" {
		t.Error("should verify valid key")
	}

	_, ok, _ = s.VerifyTenantMasterKey(ctx, "wrong-key")
	if ok {
		t.Error("should not verify wrong key")
	}
}

func TestTenantUsers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	s.CreateTenantUser(ctx, "user1", "t1", "hash1")
	users, _ := s.ListTenantUsers(ctx, "t1")
	if len(users) != 1 || users[0].Username != "user1" {
		t.Error("expected user1")
	}

	tid, hash, ok, _ := s.LookupTenantUser(ctx, "user1")
	if !ok || tid != "t1" || hash != "hash1" {
		t.Error("should lookup user")
	}

	s.DeleteTenantUser(ctx, "user1")
	users, _ = s.ListTenantUsers(ctx, "t1")
	if len(users) != 0 {
		t.Error("user should be deleted")
	}
}
