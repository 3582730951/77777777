// session.go — resolves a stored OAuth credential blob into a usable bearer
// access token for api.anthropic.com.
//
// The secret blob (written by oauth.Manager.buildClaudeSession) is JSON:
//
//	{
//	  "access_token":  "...",
//	  "refresh_token": "...",
//	  "expires_at":    "2026-01-01T00:00:00Z",
//	  "account_uuid":  "...",
//	  "email":         "..."
//	}
//
// We cache by accountID; the resolver auto-refreshes when the token is within
// 3 minutes of expiry.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	claudeTokenURL = "https://api.anthropic.com/v1/oauth/token"
	claudeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	refreshSkew    = 3 * time.Minute
)

type sessionInfo struct {
	AccessToken  string
	RefreshToken string
	Expires      time.Time
	AccountUUID  string
	Organization string
	Email        string
}

// QuotaSnapshot holds usage data from /api/oauth/usage and response headers.
type QuotaSnapshot struct {
	// From /api/oauth/usage (primary source, same as Claude Code CLI)
	FiveHourUtilization int64 // 0-100 percent
	FiveHourReset       time.Time
	SevenDayUtilization int64 // 0-100 percent
	SevenDayReset       time.Time
	// Dynamic tier windows (all tiers from the API response)
	TierMap map[string]tierEntry
	// Extra usage (paid overage)
	ExtraEnabled      bool
	ExtraMonthlyLimit *float64
	ExtraUsedCredits  *float64
	ExtraUtilization  *float64
	ExtraCurrency     string
	// From response headers (secondary, updated on every real request)
	RequestLimit     int64
	RequestRemaining int64
	TokenLimit       int64
	TokenRemaining   int64
	ResetAt          time.Time
	UpdatedAt        time.Time
}

type tierEntry struct {
	Utilization float64
	ResetsAt    time.Time
}

type sessionResolver struct {
	mu          sync.Mutex
	cache       map[string]sessionInfo
	quotas      map[string]QuotaSnapshot
	refreshing  map[string]bool      // per-account refresh lock to prevent concurrent refreshes
	lastRefresh map[string]time.Time // throttle refresh frequency
	httpClient  *http.Client
}

func newSessionResolver() *sessionResolver {
	return &sessionResolver{
		cache:       make(map[string]sessionInfo),
		quotas:      make(map[string]QuotaSnapshot),
		refreshing:  make(map[string]bool),
		lastRefresh: make(map[string]time.Time),
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (r *sessionResolver) UpdateQuota(accountID string, q QuotaSnapshot) {
	q.UpdatedAt = time.Now()
	r.mu.Lock()
	r.quotas[accountID] = q
	r.mu.Unlock()
}

func (r *sessionResolver) GetQuota(accountID string) (QuotaSnapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	q, ok := r.quotas[accountID]
	return q, ok
}

// Resolve returns a valid access token. sessionJSON is the blob stored at
// enrollment time; refreshToken may be empty (falls back to value in JSON).
func (r *sessionResolver) Resolve(ctx context.Context, accountID, sessionJSON, refreshToken string) (sessionInfo, error) {
	r.mu.Lock()
	cached, ok := r.cache[accountID]
	r.mu.Unlock()

	if ok && time.Until(cached.Expires) > refreshSkew {
		return cached, nil
	}

	// Parse the stored JSON first.
	info, err := parseClaudeSessionJSON([]byte(sessionJSON))
	if err != nil {
		if refreshToken == "" {
			return sessionInfo{}, err
		}
		info = sessionInfo{RefreshToken: refreshToken}
	}
	// refreshToken arg overrides what's in JSON if provided.
	if refreshToken != "" {
		info.RefreshToken = refreshToken
	}

	// If still fresh, cache and return.
	if time.Until(info.Expires) > refreshSkew {
		r.mu.Lock()
		r.cache[accountID] = info
		r.mu.Unlock()
		return info, nil
	}

	// Need to refresh — but throttle to max once per 5 minutes per account
	// to avoid abnormal refresh frequency that could trigger detection.
	if info.RefreshToken == "" {
		return sessionInfo{}, errors.New("claude: access token expired and no refresh_token available")
	}
	r.mu.Lock()
	if r.refreshing[accountID] {
		r.mu.Unlock()
		// Another goroutine is already refreshing; return current token if usable.
		if time.Until(info.Expires) > 0 {
			return info, nil
		}
		return sessionInfo{}, errors.New("claude: token expired, refresh in progress")
	}
	if last, ok := r.lastRefresh[accountID]; ok && time.Since(last) < 5*time.Minute {
		r.mu.Unlock()
		if time.Until(info.Expires) > 0 {
			return info, nil
		}
		return sessionInfo{}, errors.New("claude: token expired, refresh throttled")
	}
	r.refreshing[accountID] = true
	r.mu.Unlock()

	refreshed, err := r.doRefresh(ctx, info.RefreshToken)

	r.mu.Lock()
	delete(r.refreshing, accountID)
	r.lastRefresh[accountID] = time.Now()
	r.mu.Unlock()
	if err != nil {
		// If refresh fails but original token still has some life, use it.
		if time.Until(info.Expires) > 0 {
			return info, nil
		}
		return sessionInfo{}, fmt.Errorf("refresh token: %w", err)
	}
	// Preserve metadata from original.
	refreshed.AccountUUID = info.AccountUUID
	refreshed.Organization = info.Organization
	refreshed.Email = info.Email

	r.mu.Lock()
	r.cache[accountID] = refreshed
	r.mu.Unlock()
	return refreshed, nil
}

func parseClaudeSessionJSON(body []byte) (sessionInfo, error) {
	var raw struct {
		AccessToken  string          `json:"access_token"`
		RefreshToken string          `json:"refresh_token"`
		ExpiresAt    json.RawMessage `json:"expires_at"`
		AccountUUID  string          `json:"account_uuid"`
		Organization string          `json:"organization_uuid"`
		Email        string          `json:"email"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return sessionInfo{}, fmt.Errorf("parse claude session json: %w", err)
	}
	if raw.AccessToken == "" {
		return sessionInfo{}, errors.New("no access_token in claude session blob")
	}
	exp := time.Now().Add(55 * time.Minute) // default
	if len(raw.ExpiresAt) > 0 {
		exp = parseExpiresAt(raw.ExpiresAt, exp)
	}
	return sessionInfo{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		Expires:      exp,
		AccountUUID:  raw.AccountUUID,
		Organization: raw.Organization,
		Email:        raw.Email,
	}, nil
}

// parseExpiresAt supports RFC3339 strings, Unix timestamps (seconds or
// milliseconds), matching cc-switch's is_token_expired logic.
func parseExpiresAt(raw json.RawMessage, fallback time.Time) time.Time {
	// Try as string (RFC3339).
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
		return fallback
	}
	// Try as number (Unix timestamp).
	var n float64
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		ts := int64(n)
		if ts > 1_000_000_000_000 {
			ts /= 1000 // milliseconds → seconds
		}
		return time.Unix(ts, 0)
	}
	return fallback
}

func (r *sessionResolver) doRefresh(ctx context.Context, refreshToken string) (sessionInfo, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", claudeClientID)

	req, err := http.NewRequestWithContext(ctx, "POST", claudeTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return sessionInfo{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.1.92 (external, cli)")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return sessionInfo{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return sessionInfo{}, fmt.Errorf("refresh %d: %s", resp.StatusCode, snippetB(body))
	}

	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return sessionInfo{}, fmt.Errorf("decode refresh response: %w", err)
	}
	if tok.AccessToken == "" {
		return sessionInfo{}, errors.New("empty access_token in refresh response")
	}
	exp := time.Now().Add(55 * time.Minute)
	if tok.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	rt := tok.RefreshToken
	if rt == "" {
		rt = refreshToken // reuse old one if provider didn't rotate
	}
	return sessionInfo{
		AccessToken:  tok.AccessToken,
		RefreshToken: rt,
		Expires:      exp,
	}, nil
}
