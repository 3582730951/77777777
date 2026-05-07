// session.go — resolves a stored Google OAuth credential blob into a valid
// access token for the Gemini CLI Code Assist API.
//
// The secret blob (written by oauth.Manager.buildGeminiSession) is JSON:
//
//	{
//	  "access_token":  "ya29...",
//	  "refresh_token": "1//...",
//	  "expires_at":    "2026-01-01T00:30:00Z",
//	  "email":         "user@gmail.com"
//	}
package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	geminiRefreshSkew = 3 * time.Minute
)

type sessionInfo struct {
	AccessToken  string
	RefreshToken string
	Expires      time.Time
	Email        string
	IDToken      string
}

type sessionResolver struct {
	mu         sync.Mutex
	cache      map[string]sessionInfo
	httpClient *http.Client
}

func newSessionResolver() *sessionResolver {
	return &sessionResolver{
		cache:      make(map[string]sessionInfo),
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (r *sessionResolver) Resolve(ctx context.Context, accountID, sessionJSON, refreshToken string) (sessionInfo, error) {
	r.mu.Lock()
	cached, ok := r.cache[accountID]
	r.mu.Unlock()

	if ok && time.Until(cached.Expires) > geminiRefreshSkew {
		return cached, nil
	}

	info, err := parseGeminiSessionJSON([]byte(sessionJSON))
	if err != nil {
		if refreshToken == "" {
			return sessionInfo{}, err
		}
		info = sessionInfo{RefreshToken: refreshToken}
	}
	if refreshToken != "" {
		info.RefreshToken = refreshToken
	}

	if time.Until(info.Expires) > geminiRefreshSkew {
		r.mu.Lock()
		r.cache[accountID] = info
		r.mu.Unlock()
		return info, nil
	}

	if info.RefreshToken == "" {
		return sessionInfo{}, errors.New("gemini: access token expired and no refresh_token available")
	}
	refreshed, err := r.doRefresh(ctx, info.RefreshToken)
	if err != nil {
		if time.Until(info.Expires) > 0 {
			return info, nil
		}
		return sessionInfo{}, fmt.Errorf("refresh token: %w", err)
	}
	refreshed.Email = info.Email
	refreshed.IDToken = info.IDToken

	r.mu.Lock()
	r.cache[accountID] = refreshed
	r.mu.Unlock()
	return refreshed, nil
}

func parseGeminiSessionJSON(body []byte) (sessionInfo, error) {
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresAt    string `json:"expires_at"`
		Email        string `json:"email"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return sessionInfo{}, fmt.Errorf("parse gemini session json: %w", err)
	}
	if raw.AccessToken == "" {
		return sessionInfo{}, errors.New("no access_token in gemini session blob")
	}
	exp := time.Now().Add(55 * time.Minute)
	if raw.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, raw.ExpiresAt); err == nil {
			exp = t
		}
	}
	return sessionInfo{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		Expires:      exp,
		Email:        raw.Email,
		IDToken:      raw.IDToken,
	}, nil
}

func (r *sessionResolver) doRefresh(ctx context.Context, refreshToken string) (sessionInfo, error) {
	clientID, clientSecret := geminiOAuthClient()
	if clientID == "" || clientSecret == "" {
		return sessionInfo{}, errors.New("gemini oauth client env missing: set GEMINI_OAUTH_CLIENT_ID and GEMINI_OAUTH_CLIENT_SECRET")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)

	req, err := http.NewRequestWithContext(ctx, "POST", googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return sessionInfo{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", geminiCLIUserAgent)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return sessionInfo{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return sessionInfo{}, fmt.Errorf("google refresh %d: %s", resp.StatusCode, snippetG(b))
	}

	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(b, &tok); err != nil {
		return sessionInfo{}, fmt.Errorf("decode google refresh: %w", err)
	}
	if tok.AccessToken == "" {
		return sessionInfo{}, errors.New("empty access_token in google refresh response")
	}
	exp := time.Now().Add(55 * time.Minute)
	if tok.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	rt := tok.RefreshToken
	if rt == "" {
		rt = refreshToken
	}
	return sessionInfo{
		AccessToken:  tok.AccessToken,
		RefreshToken: rt,
		Expires:      exp,
	}, nil
}

func geminiOAuthClient() (clientID, clientSecret string) {
	clientID = firstGeminiEnv("GEMINI_OAUTH_CLIENT_ID", "GOOGLE_OAUTH_CLIENT_ID")
	clientSecret = firstGeminiEnv("GEMINI_OAUTH_CLIENT_SECRET", "GOOGLE_OAUTH_CLIENT_SECRET")
	return clientID, clientSecret
}

func firstGeminiEnv(keys ...string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return ""
}
