// session.go — resolves a stored credential blob into a usable bearer access
// token. We accept two flavours of input (set by /accounts/new):
//
//  1. The full /api/auth/session JSON (recommended): start with `{`. We can
//     read the accessToken directly and only refresh when its JWT exp claim is
//     close to firing.
//
//  2. The raw __Secure-next-auth.session-token JWE cookie value: start with
//     `eyJ`. We GET https://chatgpt.com/api/auth/session ourselves, with the
//     cookie set, parse the JSON, and use the accessToken returned.
//
// The cache is keyed by account id; we persist nothing here (refreshing on
// startup is fine).
package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type sessionInfo struct {
	AccessToken  string
	RefreshToken string
	Expires      time.Time
	AccountID    string
	PlanType     string
}

type sessionResolver struct {
	httpClient *http.Client
	cache      sync.Map // accountID -> sessionInfo
	refreshFn  func(ctx context.Context, refreshToken string) (newAccess, newRefresh, idToken string, expiresIn int, err error)
}

// SetRefreshFunc wires the OAuth refresh callback. Optional — when nil, expired
// access tokens cause Resolve to fail (caller should re-enroll).
func (r *sessionResolver) SetRefreshFunc(f func(ctx context.Context, refreshToken string) (string, string, string, int, error)) {
	r.refreshFn = f
}

func newSessionResolver() *sessionResolver {
	return &sessionResolver{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// Resolve returns a non-expired access token for the supplied raw secret. The
// secret can be either the JSON session blob (Format 1) or the raw next-auth
// cookie (Format 2). accountID is just a cache key; pass any stable string.
func (r *sessionResolver) Resolve(ctx context.Context, accountID, secret, ua string) (sessionInfo, error) {
	if cached, ok := r.cache.Load(accountID); ok {
		if info, ok := cached.(sessionInfo); ok && time.Until(info.Expires) > 2*time.Minute {
			log.Printf("[chatgpt-session] account=%s cache hit, expires=%v, access_token_len=%d", accountID, info.Expires, len(info.AccessToken))
			return info, nil
		}
	}
	// Try refresh first if we have a refresh_token from a previous fetch.
	if cached, ok := r.cache.Load(accountID); ok && r.refreshFn != nil {
		if info, ok := cached.(sessionInfo); ok && info.RefreshToken != "" {
			newAccess, newRefresh, idTok, expIn, err := r.refreshFn(ctx, info.RefreshToken)
			if err == nil && newAccess != "" {
				exp := time.Now().Add(time.Duration(expIn) * time.Second)
				if exp.IsZero() || expIn == 0 {
					exp = jwtExpiry(newAccess)
					if exp.IsZero() {
						exp = time.Now().Add(50 * time.Minute)
					}
				}
				updated := sessionInfo{
					AccessToken:  newAccess,
					RefreshToken: newRefresh,
					Expires:      exp,
					AccountID:    info.AccountID,
					PlanType:     info.PlanType,
				}
				r.cache.Store(accountID, updated)
				_ = idTok
				return updated, nil
			}
		}
	}
	log.Printf("[chatgpt-session] account=%s cache miss/expired, fetching fresh (secret_len=%d)", accountID, len(secret))
	info, err := r.fetchFresh(ctx, secret, ua)
	if err != nil {
		log.Printf("[chatgpt-session] account=%s fetchFresh error: %v", accountID, err)
		return sessionInfo{}, err
	}
	log.Printf("[chatgpt-session] account=%s fetchFresh ok: access_token_len=%d, account_id=%q, plan=%q, expires=%v",
		accountID, len(info.AccessToken), info.AccountID, info.PlanType, info.Expires)
	r.cache.Store(accountID, info)
	return info, nil
}

func (r *sessionResolver) fetchFresh(ctx context.Context, secret, ua string) (sessionInfo, error) {
	s := strings.TrimSpace(secret)
	if s == "" {
		return sessionInfo{}, errors.New("empty session token")
	}
	if strings.HasPrefix(s, "{") {
		return parseSessionJSON([]byte(s))
	}
	// Raw cookie path — GET /api/auth/session ourselves.
	req, err := http.NewRequestWithContext(ctx, "GET", "https://chatgpt.com/api/auth/session", nil)
	if err != nil {
		return sessionInfo{}, err
	}
	if ua == "" {
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Cookie", "__Secure-next-auth.session-token="+s)
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return sessionInfo{}, fmt.Errorf("auth/session: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		bodyLower := strings.ToLower(string(body))
		if strings.Contains(bodyLower, "deactivated") || strings.Contains(bodyLower, "banned") || strings.Contains(bodyLower, "suspended") {
			return sessionInfo{}, fmt.Errorf("account banned: status=%d %s", resp.StatusCode, snippet(body))
		}
		return sessionInfo{}, fmt.Errorf("auth/session %d: %s", resp.StatusCode, snippet(body))
	}
	return parseSessionJSON(body)
}

func parseSessionJSON(body []byte) (sessionInfo, error) {
	var raw struct {
		User struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"user"`
		Expires string `json:"expires"`
		Account struct {
			ID       string `json:"id"`
			PlanType string `json:"planType"`
		} `json:"account"`
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		IDToken      string `json:"idToken"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return sessionInfo{}, fmt.Errorf("parse session json: %w", err)
	}
	if raw.AccessToken == "" {
		// Check for ban/deactivation signals in the raw JSON
		bodyLower := strings.ToLower(string(body))
		if strings.Contains(bodyLower, "account_deactivated") ||
			strings.Contains(bodyLower, "workspace_deactivated") ||
			strings.Contains(bodyLower, "deactivated") ||
			strings.Contains(bodyLower, "suspended") ||
			strings.Contains(bodyLower, "banned") {
			return sessionInfo{}, fmt.Errorf("account banned: %s", snippet(body))
		}
		return sessionInfo{}, errors.New("no accessToken in session response")
	}
	exp := jwtExpiry(raw.AccessToken)
	if exp.IsZero() {
		if t, err := time.Parse(time.RFC3339, raw.Expires); err == nil {
			exp = t
		} else {
			exp = time.Now().Add(5 * time.Minute)
		}
	}
	return sessionInfo{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		Expires:      exp,
		AccountID:    raw.Account.ID,
		PlanType:     raw.Account.PlanType,
	}, nil
}

// jwtExpiry decodes a JWT's payload (without verifying signature) and returns
// the `exp` claim as a Time. Returns zero on any failure.
func jwtExpiry(jwt string) time.Time {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claim struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claim); err != nil {
		return time.Time{}
	}
	if claim.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claim.Exp, 0)
}

func snippet(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
