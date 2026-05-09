package kiro

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/store"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestEnsureAccessTokenSingleflightsRefreshAndPersistsRotation(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "kiro.db"), "test-master-key")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	acc := &domain.Account{
		ID:       "kiro-acc-1",
		TenantID: "tenant-1",
		Provider: "kiro",
		State:    domain.StateActive,
	}
	if err := st.UpsertAccount(ctx, acc, store.AccountSecret{RefreshToken: "initial-refresh"}); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	p := New("real")
	p.SetStore(st)
	p.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.URL.String() != refreshURL {
			t.Errorf("unexpected refresh URL: %s", req.URL.String())
		}
		body, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(body), "initial-refresh") {
			t.Errorf("refresh request did not use stored refresh token: %s", string(body))
		}
		time.Sleep(25 * time.Millisecond)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"accessToken":"access-1",
				"refreshToken":"rotated-refresh",
				"profileArn":"profile-arn-1",
				"expiresIn":3600
			}`)),
		}, nil
	})}

	start := make(chan struct{})
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			accessToken, profileArn, err := p.ensureAccessToken(ctx, acc)
			if err != nil {
				errs <- err
				return
			}
			if accessToken != "access-1" || profileArn != "profile-arn-1" {
				errs <- fmt.Errorf("unexpected token result: access=%q profile=%q", accessToken, profileArn)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("expected one upstream refresh, got %d", got)
	}

	sec, err := st.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sec.RefreshToken != "rotated-refresh" {
		t.Fatalf("rotated refresh token was not persisted: %q", sec.RefreshToken)
	}

	accessToken, profileArn, err := p.ensureAccessToken(ctx, acc)
	if err != nil {
		t.Fatal(err)
	}
	if accessToken != "access-1" || profileArn != "profile-arn-1" {
		t.Fatalf("unexpected cached token result: access=%q profile=%q", accessToken, profileArn)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("cached token should not trigger another refresh, got %d calls", got)
	}
}

func TestEnsureAccessTokenUsesStoredProfileArnWhenOIDCRefreshOmitsIt(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "kiro.db"), "test-master-key")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	acc := &domain.Account{
		ID:       "kiro-acc-oidc",
		TenantID: "tenant-1",
		Provider: "kiro",
		State:    domain.StateActive,
	}
	sec := store.AccountSecret{
		RefreshToken: "oidc-refresh",
		Cookies:      []byte(`{"client_id":"cid","client_secret":"secret","profile_arn":"stored-profile"}`),
	}
	if err := st.UpsertAccount(ctx, acc, sec); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	p := New("real")
	p.SetStore(st)
	p.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.URL.String() != oidcRefreshURL {
			t.Errorf("unexpected refresh URL: %s", req.URL.String())
		}
		body, _ := io.ReadAll(req.Body)
		bodyText := string(body)
		for _, want := range []string{"cid", "secret", "oidc-refresh"} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("refresh request missing %q: %s", want, bodyText)
			}
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"accessToken":"access-oidc",
				"refreshToken":"oidc-refresh-rotated",
				"expiresIn":3600
			}`)),
		}, nil
	})}

	accessToken, profileArn, err := p.ensureAccessToken(ctx, acc)
	if err != nil {
		t.Fatal(err)
	}
	if accessToken != "access-oidc" {
		t.Fatalf("access token = %q", accessToken)
	}
	if profileArn != "stored-profile" {
		t.Fatalf("profile arn = %q, want stored-profile", profileArn)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}
