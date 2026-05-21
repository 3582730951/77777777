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
// The cache is keyed by account id. The provider persists refreshed sessions
// after Resolve returns a rotated token.
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
	Email        string
	IDToken      string
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
func (r *sessionResolver) Resolve(ctx context.Context, accountID, secret, refreshToken, ua string) (sessionInfo, error) {
	currentRefreshToken := chatGPTPreferredRefreshToken(chatGPTRefreshTokenFromSession(secret), strings.TrimSpace(refreshToken))
	if cached, ok := r.cache.Load(accountID); ok {
		if info, ok := cached.(sessionInfo); ok && time.Until(info.Expires) > 2*time.Minute {
			if currentRefreshToken != "" && currentRefreshToken != info.RefreshToken {
				if fresh, ok := parseStoredSessionSecret(secret, refreshToken); ok && time.Until(fresh.Expires) > 2*time.Minute {
					log.Printf("[chatgpt-session] account=%s cache bypassed; store has rotated session", accountID)
					r.cache.Store(accountID, fresh)
					return fresh, nil
				}
				// Another process/request may have already rotated and persisted the
				// refresh token. Keep serving the still-valid access token, but update
				// the in-memory refresh token so the next refresh does not reuse the old
				// one.
				log.Printf("[chatgpt-session] account=%s cache hit with newer stored refresh token", accountID)
				info.RefreshToken = currentRefreshToken
				r.cache.Store(accountID, info)
			}
			log.Printf("[chatgpt-session] account=%s cache hit, expires=%v, access_token_len=%d", accountID, info.Expires, len(info.AccessToken))
			return info, nil
		}
	}
	// Try refresh first if we have a refresh_token from a previous fetch.
	if cached, ok := r.cache.Load(accountID); ok && r.refreshFn != nil {
		if info, ok := cached.(sessionInfo); ok && info.RefreshToken != "" {
			if currentRefreshToken != "" && currentRefreshToken != info.RefreshToken {
				log.Printf("[chatgpt-session] account=%s skip stale cached refresh token; stored token rotated", accountID)
				r.cache.Delete(accountID)
			} else {
				updated, err := r.refreshAny(ctx, info, refreshTokenCandidates(
					chatGPTRefreshTokenFromSession(secret),
					strings.TrimSpace(refreshToken),
					info.RefreshToken,
				)...)
				if err == nil {
					r.cache.Store(accountID, updated)
					return updated, nil
				}
			}
		}
	}
	if strings.TrimSpace(secret) == "" && refreshToken != "" && r.refreshFn != nil {
		updated, err := r.refresh(ctx, sessionInfo{RefreshToken: refreshToken})
		if err != nil {
			return sessionInfo{}, fmt.Errorf("refresh token: %w", err)
		}
		r.cache.Store(accountID, updated)
		return updated, nil
	}
	log.Printf("[chatgpt-session] account=%s cache miss/expired, fetching fresh (secret_len=%d)", accountID, len(secret))
	info, err := r.fetchFresh(ctx, secret, ua)
	if err != nil {
		if refreshToken != "" && r.refreshFn != nil {
			updated, refreshErr := r.refreshAny(ctx, sessionInfo{}, refreshTokenCandidates(
				chatGPTRefreshTokenFromSession(secret),
				strings.TrimSpace(refreshToken),
			)...)
			if refreshErr == nil {
				r.cache.Store(accountID, updated)
				return updated, nil
			}
		}
		log.Printf("[chatgpt-session] account=%s fetchFresh error: %v", accountID, err)
		return sessionInfo{}, err
	}
	if refreshToken != "" && info.RefreshToken == "" {
		info.RefreshToken = refreshToken
	}
	log.Printf("[chatgpt-session] account=%s fetchFresh ok: access_token_len=%d, account_id=%q, plan=%q, expires=%v",
		accountID, len(info.AccessToken), info.AccountID, info.PlanType, info.Expires)

	if time.Until(info.Expires) <= 2*time.Minute {
		if r.refreshFn == nil || info.RefreshToken == "" {
			if time.Until(info.Expires) > 0 {
				r.cache.Store(accountID, info)
				return info, nil
			}
			return sessionInfo{}, errors.New("chatgpt: access token expired and no refresh_token available")
		}
		updated, err := r.refreshAny(ctx, info, refreshTokenCandidates(
			chatGPTRefreshTokenFromSession(secret),
			strings.TrimSpace(refreshToken),
			info.RefreshToken,
		)...)
		if err != nil {
			if time.Until(info.Expires) > 0 {
				r.cache.Store(accountID, info)
				return info, nil
			}
			return sessionInfo{}, fmt.Errorf("refresh token: %w", err)
		}
		r.cache.Store(accountID, updated)
		return updated, nil
	}
	r.cache.Store(accountID, info)
	return info, nil
}

// ForceRefresh bypasses any still-valid cached access token and refreshes with
// the newest refresh token currently stored for the account. This is used after
// the Codex backend returns token_invalidated before the JWT exp claim.
func (r *sessionResolver) ForceRefresh(ctx context.Context, accountID, secret, refreshToken string) (sessionInfo, error) {
	info, _ := parseStoredSessionSecret(secret, refreshToken)
	var cachedInfo sessionInfo
	if cached, ok := r.cache.Load(accountID); ok {
		if ci, ok := cached.(sessionInfo); ok {
			cachedInfo = ci
			if info.AccessToken == "" {
				info = cachedInfo
			}
		}
	}
	candidates := refreshTokenCandidates(
		chatGPTRefreshTokenFromSession(secret),
		strings.TrimSpace(refreshToken),
		info.RefreshToken,
		cachedInfo.RefreshToken,
	)
	if len(candidates) == 0 {
		r.cache.Delete(accountID)
		return sessionInfo{}, errors.New("chatgpt: token invalidated and no refresh_token available")
	}
	updated, err := r.refreshAny(ctx, info, candidates...)
	if err != nil {
		r.cache.Delete(accountID)
		return sessionInfo{}, err
	}
	r.cache.Store(accountID, updated)
	return updated, nil
}

func (r *sessionResolver) refreshAny(ctx context.Context, info sessionInfo, candidates ...string) (sessionInfo, error) {
	candidates = refreshTokenCandidates(append([]string{info.RefreshToken}, candidates...)...)
	if len(candidates) == 0 {
		return sessionInfo{}, errors.New("empty refresh_token")
	}
	var lastErr error
	for i, token := range candidates {
		next := info
		next.RefreshToken = token
		updated, err := r.refresh(ctx, next)
		if err == nil {
			return updated, nil
		}
		lastErr = err
		if i+1 >= len(candidates) || !isChatGPTRefreshReuseError(err) {
			break
		}
		log.Printf("[chatgpt-session] refresh token rejected as reused; trying alternate stored token")
	}
	if lastErr == nil {
		lastErr = errors.New("empty refresh_token")
	}
	return sessionInfo{}, lastErr
}

func refreshTokenCandidates(values ...string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func parseStoredSessionSecret(secret, fallbackRefreshToken string) (sessionInfo, bool) {
	trimmed := strings.TrimSpace(secret)
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return sessionInfo{}, false
	}
	info, err := parseSessionJSON([]byte(trimmed))
	if err != nil {
		return sessionInfo{}, false
	}
	if info.RefreshToken == "" {
		info.RefreshToken = strings.TrimSpace(fallbackRefreshToken)
	}
	return info, true
}

func (r *sessionResolver) refresh(ctx context.Context, info sessionInfo) (sessionInfo, error) {
	if r.refreshFn == nil {
		return sessionInfo{}, errors.New("refresh callback not configured")
	}
	if strings.TrimSpace(info.RefreshToken) == "" {
		return sessionInfo{}, errors.New("empty refresh_token")
	}
	newAccess, newRefresh, idTok, expIn, err := r.refreshFn(ctx, info.RefreshToken)
	if err != nil {
		return sessionInfo{}, err
	}
	if newAccess == "" {
		return sessionInfo{}, errors.New("empty access_token in refresh response")
	}
	exp := time.Time{}
	if expIn > 0 {
		exp = time.Now().Add(time.Duration(expIn) * time.Second)
	}
	if exp.IsZero() {
		exp = jwtExpiry(newAccess)
		if exp.IsZero() {
			exp = time.Now().Add(50 * time.Minute)
		}
	}
	if newRefresh == "" {
		newRefresh = info.RefreshToken
	}
	if idTok == "" {
		idTok = info.IDToken
	}
	accountID := info.AccountID
	planType := info.PlanType
	email := info.Email
	if idTok != "" {
		claimAccount, claimPlan, claimEmail := codexClaims(idTok)
		if accountID == "" {
			accountID = claimAccount
		}
		if planType == "" {
			planType = claimPlan
		}
		if email == "" {
			email = claimEmail
		}
	}
	return sessionInfo{
		AccessToken:  newAccess,
		RefreshToken: newRefresh,
		Expires:      exp,
		AccountID:    accountID,
		PlanType:     planType,
		Email:        email,
		IDToken:      idTok,
	}, nil
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
		Email:        raw.User.Email,
		IDToken:      raw.IDToken,
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

func codexClaims(jwt string) (accountID, planType, email string) {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(addB64Padding(parts[1]))
		if err != nil {
			return
		}
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(payload, &nested); err != nil {
		return
	}
	if a, ok := nested["https://api.openai.com/auth"]; ok {
		var inner struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
			ChatGPTPlanType  string `json:"chatgpt_plan_type"`
		}
		_ = json.Unmarshal(a, &inner)
		accountID = inner.ChatGPTAccountID
		planType = inner.ChatGPTPlanType
	}
	if e, ok := nested["email"]; ok {
		_ = json.Unmarshal(e, &email)
	}
	return
}

func addB64Padding(s string) string {
	switch len(s) % 4 {
	case 2:
		return s + "=="
	case 3:
		return s + "="
	}
	return s
}

func snippet(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
