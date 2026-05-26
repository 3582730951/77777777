// Package chatgpt is the provider adapter for chatgpt.com (OpenAI subscription
// web). MVP supports two modes:
//
//   - mock: deterministic test response (default)
//   - real: actual reverse-engineered call to /backend-api/conversation
//
// Real mode handles two credential formats (see session.go) and parses the
// JSON-Patch SSE delta protocol (see stream.go) into IR events.
package chatgpt

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/proxypool"
	"github.com/llm-pool/gateway/internal/scheduler"
	"github.com/llm-pool/gateway/internal/store"
	"github.com/llm-pool/gateway/internal/transport"
)

const (
	ModeMock = "mock"
	ModeReal = "real"

	defaultCodexUserAgent  = "codex_cli_rs/0.45.0 (Linux; x86_64) Codex/1.0"
	defaultCodexOriginator = "codex_cli_rs"
	cpaCodexUserAgent      = "codex-tui/0.118.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.118.0)"
	cpaCodexOriginator     = "codex-tui"
)

type Provider struct {
	Mode       string
	store      *store.Store
	resolver   *sessionResolver
	httpClient *http.Client
	mu         sync.Mutex
	resolves   map[string]*sessionResolveCall
	proxyPool  *proxypool.Manager
	clientMu   sync.Mutex
	clients    map[string]*http.Client
	// sessionLocks serializes every session mutation path for one account. This
	// mirrors Codex-Manager/sub2api's "lock -> reread persisted token -> refresh"
	// flow and prevents Resolve and ForceRefresh from consuming the same
	// one-time refresh_token concurrently.
	sessionLocks sync.Map // accountID -> *sync.Mutex
}

type sessionResolveCall struct {
	done chan struct{}
	info sessionInfo
	err  error
}

// SetStore wires the credential store. Required for ModeReal; harmless in mock.
func (p *Provider) SetStore(s *store.Store) { p.store = s }

// SetRefreshFunc plumbs the OAuth refresh callback to the session resolver.
func (p *Provider) SetRefreshFunc(f func(ctx context.Context, refreshToken string) (string, string, string, int, error)) {
	p.resolver.SetRefreshFunc(f)
}

func (p *Provider) SetRefreshFuncWithClient(f func(ctx context.Context, refreshToken string, client *http.Client) (string, string, string, int, error)) {
	p.resolver.SetRefreshFuncWithClient(f)
}

func (p *Provider) SetProxyPool(m *proxypool.Manager) {
	p.proxyPool = m
}

// RefreshCredential resolves the stored session and refreshes it when the
// access token is expired or close to expiry. It deliberately avoids quota
// endpoints so the background keepalive loop stays cheap.
func (p *Provider) RefreshCredential(ctx context.Context, acc *domain.Account) error {
	if p.Mode == ModeMock {
		return nil
	}
	if p.store == nil {
		return errors.New("store not wired")
	}
	sec, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		return fmt.Errorf("get secret: %w", err)
	}
	if chatGPTWebSessionRefreshDue(sec, time.Now()) {
		if _, err := p.refreshSessionFromStoredCookie(ctx, acc, sec, false); err == nil {
			return nil
		} else if !chatGPTStoredAccessTokenUsable(sec, time.Now().Add(2*time.Minute)) {
			return err
		} else {
			log.Printf("[chatgpt-session] account=%s scheduled web session refresh failed; keeping current access token: %v", acc.ID, err)
		}
	}
	_, err = p.resolveSession(ctx, acc, sec)
	return err
}

func New(mode string) *Provider {
	if mode == "" {
		mode = ModeMock
	}
	return &Provider{
		Mode:       mode,
		resolver:   newSessionResolver(),
		httpClient: transport.Codex,
		clients:    map[string]*http.Client{},
	}
}

func (p *Provider) Name() string { return "chatgpt" }

func (p *Provider) httpClientForAccount(ctx context.Context, acc *domain.Account) (*http.Client, error) {
	if p.proxyPool == nil || acc == nil {
		return p.httpClient, nil
	}
	selection, err := p.proxyPool.Resolve(ctx, acc)
	if err != nil {
		return nil, err
	}
	if selection.Direct || selection.Proxy == nil || strings.TrimSpace(selection.Proxy.URL) == "" {
		return p.httpClient, nil
	}
	return p.httpClientForProxy(selection.Proxy.URL)
}

func (p *Provider) httpClientForProxy(proxyURL string) (*http.Client, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return p.httpClient, nil
	}
	p.clientMu.Lock()
	if p.clients == nil {
		p.clients = map[string]*http.Client{}
	}
	if client := p.clients[proxyURL]; client != nil {
		p.clientMu.Unlock()
		return client, nil
	}
	p.clientMu.Unlock()

	client, err := transport.ForProviderProxy(proxyURL, transport.Options{
		MaxIdleConnsPerHost: 64,
		Timeout:             600 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	p.clientMu.Lock()
	if existing := p.clients[proxyURL]; existing != nil {
		p.clientMu.Unlock()
		return existing, nil
	}
	p.clients[proxyURL] = client
	p.clientMu.Unlock()
	return client, nil
}

func (p *Provider) resolveSession(ctx context.Context, acc *domain.Account, sec store.AccountSecret) (sessionInfo, error) {
	if acc == nil {
		return sessionInfo{}, errors.New("nil account")
	}
	return p.runSessionCall(ctx, acc.ID, func() (sessionInfo, error) {
		return p.resolveSessionOnce(ctx, acc, sec)
	})
}

func (p *Provider) forceRefreshSession(ctx context.Context, acc *domain.Account, sec store.AccountSecret) (sessionInfo, error) {
	if acc == nil {
		return sessionInfo{}, errors.New("nil account")
	}
	return p.runSessionCall(ctx, acc.ID+"\x00force-refresh", func() (sessionInfo, error) {
		return p.forceRefreshSessionOnce(ctx, acc, sec)
	})
}

func (p *Provider) runSessionCall(ctx context.Context, key string, fn func() (sessionInfo, error)) (sessionInfo, error) {
	p.mu.Lock()
	if p.resolves == nil {
		p.resolves = make(map[string]*sessionResolveCall)
	}
	if call, ok := p.resolves[key]; ok {
		done := call.done
		p.mu.Unlock()
		select {
		case <-done:
			return call.info, call.err
		case <-ctx.Done():
			return sessionInfo{}, ctx.Err()
		}
	}
	call := &sessionResolveCall{done: make(chan struct{})}
	p.resolves[key] = call
	p.mu.Unlock()

	call.info, call.err = fn()

	p.mu.Lock()
	delete(p.resolves, key)
	close(call.done)
	p.mu.Unlock()

	return call.info, call.err
}

func (p *Provider) sessionMutationLock(accountID string) *sync.Mutex {
	actual, _ := p.sessionLocks.LoadOrStore(accountID, &sync.Mutex{})
	if mu, ok := actual.(*sync.Mutex); ok {
		return mu
	}
	mu := &sync.Mutex{}
	p.sessionLocks.Store(accountID, mu)
	return mu
}

func (p *Provider) withSessionMutationLock(accountID string, fn func() (sessionInfo, error)) (sessionInfo, error) {
	mu := p.sessionMutationLock(accountID)
	mu.Lock()
	defer mu.Unlock()
	return fn()
}

func (p *Provider) resolveSessionOnce(ctx context.Context, acc *domain.Account, sec store.AccountSecret) (sessionInfo, error) {
	return p.withSessionMutationLock(acc.ID, func() (sessionInfo, error) {
		if p.store != nil {
			if fresh, err := p.store.GetAccountSecret(ctx, acc.ID); err == nil {
				sec = fresh
			}
		}
		sec = chatGPTSessionOnlySnapshotSecret(sec)
		client, err := p.httpClientForAccount(ctx, acc)
		if err != nil {
			return sessionInfo{}, err
		}
		info, err := p.resolver.Resolve(ctx, acc.ID, sec.SessionToken, sec.RefreshToken, acc.UA, client)
		if err != nil {
			if recovered, ok := p.recoverResolvedSessionRace(ctx, acc, sec, err); ok {
				return recovered, nil
			}
			p.clearStaleRefreshTokenAfterFailure(ctx, acc, sec, err)
			return sessionInfo{}, err
		}
		if err := p.persistResolvedSession(ctx, acc, sec, info); err != nil {
			return sessionInfo{}, fmt.Errorf("persist refreshed session: %w", err)
		}
		return info, nil
	})
}

func (p *Provider) forceRefreshSessionOnce(ctx context.Context, acc *domain.Account, sec store.AccountSecret) (sessionInfo, error) {
	used := chatGPTSessionOnlySnapshotSecret(sec)
	return p.withSessionMutationLock(acc.ID, func() (sessionInfo, error) {
		if p.store != nil {
			if fresh, err := p.store.GetAccountSecret(ctx, acc.ID); err == nil {
				sec = chatGPTSessionOnlySnapshotSecret(fresh)
				if chatGPTSessionMaterialChanged(used, fresh) {
					client, clientErr := p.httpClientForAccount(ctx, acc)
					if clientErr != nil {
						return sessionInfo{}, clientErr
					}
					info, resolveErr := p.resolver.Resolve(ctx, acc.ID, fresh.SessionToken, fresh.RefreshToken, acc.UA, client)
					if resolveErr == nil {
						if err := p.persistResolvedSession(ctx, acc, fresh, info); err != nil {
							return sessionInfo{}, fmt.Errorf("persist refreshed session: %w", err)
						}
						log.Printf("[chatgpt-session] account=%s force refresh skipped; persisted session already rotated", acc.ID)
						return info, nil
					}
					log.Printf("[chatgpt-session] account=%s rotated session resolve failed; forcing refresh: %v", acc.ID, resolveErr)
				}
			}
		}
		client, err := p.httpClientForAccount(ctx, acc)
		if err != nil {
			return sessionInfo{}, err
		}
		info, err := p.resolver.ForceRefresh(ctx, acc.ID, sec.SessionToken, sec.RefreshToken, client)
		if err != nil {
			var cookieRecoveryErr error
			if isChatGPTRecoverableRefreshError(err) {
				if recovered, recoverErr, ok := p.recoverSessionFromStoredCookie(ctx, acc, sec, err); ok {
					return recovered, nil
				} else {
					cookieRecoveryErr = recoverErr
				}
			}
			if recovered, ok := p.recoverResolvedSessionRace(ctx, acc, sec, err); ok {
				return recovered, nil
			}
			p.clearStaleRefreshTokenAfterFailure(ctx, acc, sec, err)
			if cookieRecoveryErr != nil {
				return sessionInfo{}, chatGPTRefreshRecoveryError(err, cookieRecoveryErr)
			}
			return sessionInfo{}, err
		}
		if err := p.persistResolvedSession(ctx, acc, sec, info); err != nil {
			return sessionInfo{}, fmt.Errorf("persist refreshed session: %w", err)
		}
		return info, nil
	})
}

func chatGPTCanRecoverAuthFailure(sec store.AccountSecret) bool {
	if IsSessionOnlyAuthJSON(sec.SessionToken) {
		return false
	}
	return chatGPTRefreshTokenFromSecret(sec) != "" || chatGPTCookieHeaderFromSecret(sec) != ""
}

func (p *Provider) recoverSessionFromStoredCookie(ctx context.Context, acc *domain.Account, sec store.AccountSecret, refreshErr error) (sessionInfo, error, bool) {
	info, err := p.refreshSessionFromStoredCookie(ctx, acc, sec, isChatGPTRefreshReuseError(refreshErr))
	if err != nil {
		log.Printf("[chatgpt-session] account=%s cookie recovery after refresh failure failed: refresh_err=%v cookie_err=%v", acc.ID, refreshErr, err)
		return sessionInfo{}, err, false
	}
	log.Printf("[chatgpt-session] account=%s recovered session via stored next-auth cookie after refresh failure", acc.ID)
	return info, nil, true
}

func (p *Provider) refreshSessionFromStoredCookie(ctx context.Context, acc *domain.Account, sec store.AccountSecret, clearRefreshToken bool) (sessionInfo, error) {
	cookieHeader := chatGPTCookieHeaderFromSecret(sec)
	if cookieHeader == "" {
		return sessionInfo{}, errors.New("no stored next-auth cookie available")
	}
	client, clientErr := p.httpClientForAccount(ctx, acc)
	if clientErr != nil {
		return sessionInfo{}, fmt.Errorf("cookie recovery proxy selection: %w", clientErr)
	}
	fallbackRefresh := sec.RefreshToken
	if clearRefreshToken {
		fallbackRefresh = ""
	}
	info, setCookies, err := p.resolver.FetchFreshWithCookieHeader(ctx, acc.ID, cookieHeader, fallbackRefresh, acc.UA, client)
	if err != nil {
		return sessionInfo{}, err
	}
	if err := p.persistCookieResolvedSession(ctx, acc, sec, info, setCookies, clearRefreshToken); err != nil {
		return sessionInfo{}, fmt.Errorf("persist cookie-recovered session: %w", err)
	}
	return info, nil
}

func (p *Provider) recoverResolvedSessionRace(ctx context.Context, acc *domain.Account, used store.AccountSecret, refreshErr error) (sessionInfo, bool) {
	if p.store == nil || !isChatGPTRecoverableRefreshError(refreshErr) {
		return sessionInfo{}, false
	}
	latest, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		return sessionInfo{}, false
	}
	if !chatGPTSessionMaterialChanged(used, latest) {
		return sessionInfo{}, false
	}
	client, clientErr := p.httpClientForAccount(ctx, acc)
	if clientErr != nil {
		return sessionInfo{}, false
	}
	info, err := p.resolver.Resolve(ctx, acc.ID, latest.SessionToken, latest.RefreshToken, acc.UA, client)
	if err != nil {
		return sessionInfo{}, false
	}
	if err := p.persistResolvedSession(ctx, acc, latest, info); err != nil {
		log.Printf("[chatgpt-session] account=%s persist race-recovered session: %v", acc.ID, err)
	}
	return info, true
}

func (p *Provider) clearStaleRefreshTokenAfterFailure(ctx context.Context, acc *domain.Account, used store.AccountSecret, refreshErr error) {
	if p.store == nil || acc == nil || !isChatGPTRefreshReuseError(refreshErr) {
		return
	}
	staleTokens := refreshTokenCandidates(
		chatGPTRefreshTokenFromSession(used.SessionToken),
		strings.TrimSpace(used.RefreshToken),
	)
	if cached, ok := p.resolver.cache.Load(acc.ID); ok {
		if info, ok := cached.(sessionInfo); ok {
			staleTokens = refreshTokenCandidates(append(staleTokens, info.RefreshToken)...)
		}
	}
	if len(staleTokens) == 0 {
		return
	}

	latest := used
	if fresh, err := p.store.GetAccountSecret(ctx, acc.ID); err == nil {
		latest = fresh
	}
	next, changed := chatGPTClearMatchingRefreshTokens(latest, staleTokens)
	if !changed {
		return
	}

	writeAcc := acc
	if freshAcc, err := p.store.GetAccount(ctx, acc.ID); err == nil {
		writeAcc = freshAcc
	}
	writeAcc.UpdatedAt = time.Now()
	if err := p.store.UpsertAccount(ctx, writeAcc, next); err != nil {
		log.Printf("[chatgpt-session] account=%s clear reused refresh token failed: %v", acc.ID, err)
		return
	}
	p.resolver.cache.Delete(acc.ID)
	log.Printf("[chatgpt-session] account=%s cleared reused refresh token from persisted credentials", acc.ID)
}

func chatGPTClearMatchingRefreshTokens(sec store.AccountSecret, staleTokens []string) (store.AccountSecret, bool) {
	stale := make(map[string]struct{}, len(staleTokens))
	for _, token := range staleTokens {
		token = strings.TrimSpace(token)
		if token != "" {
			stale[token] = struct{}{}
		}
	}
	if len(stale) == 0 {
		return sec, false
	}

	next := sec
	changed := false
	if _, ok := stale[strings.TrimSpace(next.RefreshToken)]; ok {
		next.RefreshToken = ""
		changed = true
	}
	trimmed := strings.TrimSpace(next.SessionToken)
	if strings.HasPrefix(trimmed, "{") {
		if info, ok := parseStoredSessionSecret(trimmed, ""); ok {
			if _, staleSession := stale[strings.TrimSpace(info.RefreshToken)]; staleSession {
				info.RefreshToken = ""
				next.SessionToken = buildChatGPTSessionJSON(info)
				changed = true
			}
		}
	}
	return next, changed
}

func isChatGPTRefreshReuseError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "refresh token has already been used") ||
		strings.Contains(s, "already been used to generate") ||
		strings.Contains(s, "refresh_token_reused") ||
		strings.Contains(s, "invalid_grant")
}

func isChatGPTNoRefreshTokenError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no refresh_token available") ||
		strings.Contains(s, "empty refresh_token")
}

func isChatGPTRecoverableRefreshError(err error) bool {
	return isChatGPTRefreshReuseError(err) || isChatGPTNoRefreshTokenError(err)
}

func chatGPTRefreshRecoveryError(refreshErr, cookieErr error) error {
	if cookieErr == nil {
		return refreshErr
	}
	if isChatGPTNoRefreshTokenError(refreshErr) {
		if strings.Contains(strings.ToLower(cookieErr.Error()), "no stored next-auth cookie") {
			return errors.New("chatgpt: token invalidated; no OAuth refresh_token and no stored next-auth cookie for web session recovery")
		}
		return fmt.Errorf("chatgpt: token invalidated; no OAuth refresh_token; stored web session cookie recovery failed: %w", cookieErr)
	}
	if isChatGPTRefreshReuseError(refreshErr) {
		return fmt.Errorf("chatgpt: refresh_token rejected by authority; stored web session cookie recovery failed: %w", cookieErr)
	}
	return fmt.Errorf("chatgpt: refresh failed; stored web session cookie recovery failed: %w", cookieErr)
}

func isChatGPTTokenInvalidatedError(err error) bool {
	if err == nil {
		return false
	}
	return isChatGPTTokenInvalidatedText(err.Error())
}

func isChatGPTRecoverableAuthError(err error) bool {
	if err == nil {
		return false
	}
	return isChatGPTRecoverableAuthText(err.Error())
}

func isChatGPTTokenInvalidatedResponse(status int, body []byte) bool {
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return false
	}
	return isChatGPTTokenInvalidatedText(string(body))
}

func isChatGPTRecoverableAuthResponse(status int, body []byte) bool {
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return false
	}
	return isChatGPTRecoverableAuthText(string(body))
}

func isChatGPTPlainUnauthorizedResponse(status int, body []byte) bool {
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return false
	}
	low := strings.ToLower(string(body))
	return !isChatGPTTokenInvalidatedText(low) &&
		!isChatGPTTransientUpstreamText(low) &&
		strings.Contains(low, "unauthorized")
}

func isChatGPTTokenInvalidatedText(s string) bool {
	low := strings.ToLower(s)
	if isChatGPTTransientUpstreamText(low) {
		return false
	}
	return strings.Contains(low, "token_invalidated") ||
		strings.Contains(low, "authentication token has been invalidated") ||
		strings.Contains(low, "access token has been invalidated")
}

func isChatGPTRecoverableAuthText(s string) bool {
	low := strings.ToLower(s)
	if isChatGPTTransientUpstreamText(low) {
		return false
	}
	if isChatGPTTokenInvalidatedText(s) {
		return true
	}
	return strings.Contains(low, "unauthorized") ||
		strings.Contains(low, "unauthenticated") ||
		strings.Contains(low, "invalid access token") ||
		strings.Contains(low, "invalid bearer") ||
		strings.Contains(low, "missing bearer")
}

func isChatGPTTransientUpstreamText(low string) bool {
	return strings.Contains(low, "upstream_challenge_blocked") ||
		strings.Contains(low, "cloudflare") ||
		strings.Contains(low, "cf-mitigated") ||
		strings.Contains(low, "cf_chl") ||
		strings.Contains(low, "turnstile") ||
		strings.Contains(low, "captcha") ||
		strings.Contains(low, "arkose") ||
		strings.Contains(low, "verify you are human") ||
		strings.Contains(low, "checking your browser") ||
		strings.Contains(low, "just a moment") ||
		strings.Contains(low, "chatgpt is under heavy load") ||
		strings.Contains(low, "chatgpt is at capacity") ||
		strings.Contains(low, "temporarily unavailable")
}

func chatGPTRefreshTokenChanged(oldSec, newSec store.AccountSecret) bool {
	oldSessionToken := chatGPTRefreshTokenFromSession(oldSec.SessionToken)
	newSessionToken := chatGPTRefreshTokenFromSession(newSec.SessionToken)
	if oldSessionToken != "" && newSessionToken != "" && oldSessionToken != newSessionToken {
		return true
	}
	oldTopLevel := strings.TrimSpace(oldSec.RefreshToken)
	newTopLevel := strings.TrimSpace(newSec.RefreshToken)
	if oldTopLevel != "" && newTopLevel != "" && oldTopLevel != newTopLevel {
		return true
	}
	oldToken := chatGPTPreferredRefreshToken(oldSessionToken, oldTopLevel)
	newToken := chatGPTPreferredRefreshToken(newSessionToken, newTopLevel)
	return oldToken != "" && newToken != "" && oldToken != newToken
}

func chatGPTSessionMaterialChanged(oldSec, newSec store.AccountSecret) bool {
	if chatGPTRefreshTokenChanged(oldSec, newSec) {
		return true
	}
	oldAccess := chatGPTAccessTokenFromSession(oldSec.SessionToken)
	newAccess := chatGPTAccessTokenFromSession(newSec.SessionToken)
	return oldAccess != "" && newAccess != "" && oldAccess != newAccess
}

func chatGPTRefreshTokenFromSecret(sec store.AccountSecret) string {
	if IsSessionOnlyAuthJSON(sec.SessionToken) {
		return ""
	}
	return chatGPTPreferredRefreshToken(chatGPTRefreshTokenFromSession(sec.SessionToken), strings.TrimSpace(sec.RefreshToken))
}

func chatGPTAccessTokenFromSession(sessionToken string) string {
	trimmed := strings.TrimSpace(sessionToken)
	if info, err := parseSessionJSON([]byte(trimmed)); err == nil {
		return strings.TrimSpace(info.AccessToken)
	}
	if strings.HasPrefix(trimmed, "{") {
		var raw struct {
			AccessToken string `json:"accessToken"`
			SnakeAccess string `json:"access_token"`
		}
		if err := json.Unmarshal([]byte(trimmed), &raw); err == nil {
			if raw.AccessToken != "" {
				return strings.TrimSpace(raw.AccessToken)
			}
			return strings.TrimSpace(raw.SnakeAccess)
		}
	}
	return ""
}

func chatGPTRefreshTokenFromSession(sessionToken string) string {
	trimmed := strings.TrimSpace(sessionToken)
	if IsSessionOnlyAuthJSON(trimmed) {
		return ""
	}
	if info, err := parseSessionJSON([]byte(trimmed)); err == nil {
		return strings.TrimSpace(info.RefreshToken)
	}
	if strings.HasPrefix(trimmed, "{") {
		var raw struct {
			RefreshToken string `json:"refreshToken"`
			SnakeRefresh string `json:"refresh_token"`
		}
		if err := json.Unmarshal([]byte(trimmed), &raw); err == nil {
			if raw.RefreshToken != "" {
				return strings.TrimSpace(raw.RefreshToken)
			}
			return strings.TrimSpace(raw.SnakeRefresh)
		}
	}
	return ""
}

func chatGPTPreferredRefreshToken(sessionToken, topLevelToken string) string {
	if sessionToken != "" {
		return sessionToken
	}
	return topLevelToken
}

func chatGPTNextAuthSessionCookie(sec store.AccountSecret) string {
	if st := strings.TrimSpace(sec.SessionToken); st != "" && !strings.HasPrefix(st, "{") {
		return st
	}
	return chatGPTNextAuthCookieFromBytes(sec.Cookies)
}

type chatGPTCookiePair struct {
	Name  string
	Value string
}

func chatGPTNextAuthCookieFromBytes(raw []byte) string {
	return chatGPTNextAuthCookieFromPairs(chatGPTCookiePairsFromBytes(raw))
}

// NormalizeWebSessionCookieHeader converts cookies copied from DevTools
// request headers, curl headers, Netscape cookies.txt, JSON exports, or the
// Chrome Application > Cookies table into a single Cookie header value.
func NormalizeWebSessionCookieHeader(raw string) string {
	return chatGPTCookieHeaderFromPairs(chatGPTCookiePairsFromBytes([]byte(raw)))
}

// LooksLikeWebSessionCookies reports whether the supplied cookie material
// contains a ChatGPT/NextAuth session cookie, including split .0/.1 chunks.
func LooksLikeWebSessionCookies(raw string) bool {
	return chatGPTNextAuthCookieFromBytes([]byte(raw)) != ""
}

func chatGPTCookiePairsFromBytes(raw []byte) []chatGPTCookiePair {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "{") {
		var obj map[string]any
		if json.Unmarshal([]byte(trimmed), &obj) == nil {
			pairs := make([]chatGPTCookiePair, 0, len(obj))
			for k, v := range obj {
				if s, ok := v.(string); ok {
					pairs = append(pairs, chatGPTCookiePair{Name: k, Value: s})
				}
			}
			return pairs
		}
	}
	if strings.HasPrefix(trimmed, "[") {
		var arr []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if json.Unmarshal([]byte(trimmed), &arr) == nil {
			pairs := make([]chatGPTCookiePair, 0, len(arr))
			for _, c := range arr {
				pairs = append(pairs, chatGPTCookiePair{Name: c.Name, Value: c.Value})
			}
			return pairs
		}
	}

	var pairs []chatGPTCookiePair
	for _, line := range strings.Split(strings.ReplaceAll(trimmed, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "\t") {
			if pair, ok := chatGPTCookiePairFromTabLine(line); ok {
				pairs = append(pairs, pair)
				continue
			}
		}
		parsed, recognizedHeader := chatGPTCookiePairsFromTextLine(line)
		if recognizedHeader || len(parsed) > 0 {
			pairs = append(pairs, parsed...)
		}
	}
	return pairs
}

func chatGPTCookiePairFromTabLine(line string) (chatGPTCookiePair, bool) {
	fields := strings.Split(line, "\t")
	for i := range fields {
		fields[i] = strings.TrimSpace(fields[i])
	}
	if len(fields) >= 2 && strings.EqualFold(fields[0], "name") && strings.EqualFold(fields[1], "value") {
		return chatGPTCookiePair{}, false
	}
	if len(fields) >= 7 && chatGPTBoolCookieField(fields[1]) {
		return chatGPTCookiePair{Name: fields[5], Value: fields[6]}, fields[5] != ""
	}
	if len(fields) >= 2 && chatGPTLooksLikeCookieName(fields[0]) {
		return chatGPTCookiePair{Name: fields[0], Value: fields[1]}, true
	}
	return chatGPTCookiePair{}, false
}

func chatGPTBoolCookieField(value string) bool {
	return strings.EqualFold(value, "true") || strings.EqualFold(value, "false")
}

func chatGPTLooksLikeCookieName(name string) bool {
	if name == "" || strings.EqualFold(name, "name") {
		return false
	}
	for _, r := range name {
		if r <= 0x20 || r >= 0x7f {
			return false
		}
		switch r {
		case '(', ')', '<', '>', '@', ',', ';', ':', '\\', '"', '/', '[', ']', '?', '=', '{', '}':
			return false
		}
	}
	return true
}

func chatGPTCookiePairsFromTextLine(line string) ([]chatGPTCookiePair, bool) {
	line = strings.TrimSpace(strings.Trim(line, "'\""))
	if line == "" {
		return nil, false
	}
	if strings.HasPrefix(line, "-H ") || strings.HasPrefix(line, "--header ") || strings.HasPrefix(line, "-b ") || strings.HasPrefix(line, "--cookie ") {
		if idx := strings.IndexByte(line, ' '); idx >= 0 {
			line = strings.TrimSpace(strings.Trim(line[idx+1:], "'\""))
		}
	}
	if idx := strings.Index(line, ":"); idx >= 0 {
		if eq := strings.Index(line, "="); eq >= 0 && eq < idx {
			return chatGPTCookiePairsFromCookieHeaderValue(line), false
		}
		name := strings.ToLower(strings.TrimSpace(line[:idx]))
		value := strings.TrimSpace(strings.Trim(line[idx+1:], "'\""))
		switch name {
		case "cookie":
			return chatGPTCookiePairsFromCookieHeaderValue(value), true
		case "set-cookie":
			return chatGPTCookiePairsFromSetCookieHeaderValue(value), true
		default:
			return nil, true
		}
	}
	return chatGPTCookiePairsFromCookieHeaderValue(line), false
}

func chatGPTCookiePairsFromCookieHeaderValue(value string) []chatGPTCookiePair {
	value = strings.TrimSpace(strings.Trim(value, "'\""))
	if value == "" {
		return nil
	}
	var pairs []chatGPTCookiePair
	for _, part := range strings.Split(value, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		if ok {
			pairs = append(pairs, chatGPTCookiePair{Name: strings.TrimSpace(name), Value: strings.TrimSpace(value)})
		}
	}
	return pairs
}

func chatGPTCookiePairsFromSetCookieHeaderValue(value string) []chatGPTCookiePair {
	resp := http.Response{Header: http.Header{"Set-Cookie": []string{value}}}
	cookies := resp.Cookies()
	pairs := make([]chatGPTCookiePair, 0, len(cookies))
	for _, cookie := range cookies {
		pairs = append(pairs, chatGPTCookiePair{Name: cookie.Name, Value: cookie.Value})
	}
	return pairs
}

func chatGPTCookieHeaderFromSecret(sec store.AccountSecret) string {
	if IsSessionOnlyAuthJSON(sec.SessionToken) {
		return ""
	}
	pairs := chatGPTCookiePairsFromBytes(sec.Cookies)
	if st := strings.TrimSpace(sec.SessionToken); st != "" && !strings.HasPrefix(st, "{") {
		pairs = append([]chatGPTCookiePair{{Name: "__Secure-next-auth.session-token", Value: st}}, pairs...)
	}
	if len(pairs) == 0 {
		if cookie := chatGPTNextAuthSessionCookie(sec); cookie != "" {
			pairs = []chatGPTCookiePair{{Name: "__Secure-next-auth.session-token", Value: cookie}}
		}
	}
	return chatGPTCookieHeaderFromPairs(pairs)
}

func chatGPTCookieHeaderFromPairs(pairs []chatGPTCookiePair) string {
	if len(pairs) == 0 {
		return ""
	}
	order := make([]string, 0, len(pairs))
	values := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		name := strings.TrimSpace(pair.Name)
		value := strings.TrimSpace(pair.Value)
		if name == "" || value == "" {
			continue
		}
		if _, ok := values[name]; !ok {
			order = append(order, name)
		}
		values[name] = value
	}
	if len(order) == 0 {
		return ""
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		parts = append(parts, name+"="+values[name])
	}
	return strings.Join(parts, "; ")
}

func chatGPTMergeSetCookieHeaders(existing []byte, setCookies []string) []byte {
	if len(setCookies) == 0 {
		return existing
	}
	order := make([]string, 0)
	values := map[string]string{}
	add := func(name, value string) {
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name == "" || value == "" {
			return
		}
		if _, ok := values[name]; !ok {
			order = append(order, name)
		}
		values[name] = value
	}
	for _, pair := range chatGPTCookiePairsFromBytes(existing) {
		add(pair.Name, pair.Value)
	}
	resp := http.Response{Header: http.Header{"Set-Cookie": setCookies}}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "" {
			continue
		}
		expired := cookie.MaxAge < 0 || (!cookie.Expires.IsZero() && cookie.Expires.Before(time.Now()))
		if expired {
			delete(values, cookie.Name)
			continue
		}
		add(cookie.Name, cookie.Value)
	}
	if len(values) == 0 {
		return nil
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		if value := values[name]; value != "" {
			parts = append(parts, name+"="+value)
		}
	}
	return []byte(strings.Join(parts, "; "))
}

func chatGPTNextAuthCookieFromPairs(pairs []chatGPTCookiePair) string {
	baseNames := []string{"__Secure-next-auth.session-token", "next-auth.session-token"}
	for _, base := range baseNames {
		for _, pair := range pairs {
			if pair.Name == base && strings.TrimSpace(pair.Value) != "" {
				return strings.TrimSpace(pair.Value)
			}
		}
		chunks := make(map[int]string)
		for _, pair := range pairs {
			if !strings.HasPrefix(pair.Name, base+".") {
				continue
			}
			idx, err := strconv.Atoi(strings.TrimPrefix(pair.Name, base+"."))
			if err != nil || strings.TrimSpace(pair.Value) == "" {
				continue
			}
			chunks[idx] = strings.TrimSpace(pair.Value)
		}
		if len(chunks) > 0 {
			indexes := make([]int, 0, len(chunks))
			for idx := range chunks {
				indexes = append(indexes, idx)
			}
			sort.Ints(indexes)
			var b strings.Builder
			for _, idx := range indexes {
				b.WriteString(chunks[idx])
			}
			if b.Len() > 0 {
				return b.String()
			}
		}
	}
	return ""
}

func chatGPTWebSessionRefreshDue(sec store.AccountSecret, now time.Time) bool {
	if IsSessionOnlyAuthJSON(sec.SessionToken) {
		return false
	}
	if chatGPTRefreshTokenFromSecret(sec) != "" {
		return false
	}
	if chatGPTCookieHeaderFromSecret(sec) == "" {
		return false
	}
	info, ok := parseStoredSessionSecret(sec.SessionToken, sec.RefreshToken)
	if !ok || info.AccessToken == "" || info.Expires.IsZero() {
		return true
	}
	return info.Expires.Sub(now) <= 30*time.Minute
}

func chatGPTStoredAccessTokenUsable(sec store.AccountSecret, until time.Time) bool {
	sec = chatGPTSessionOnlySnapshotSecret(sec)
	info, ok := parseStoredSessionSecret(sec.SessionToken, sec.RefreshToken)
	if !ok || info.AccessToken == "" {
		return false
	}
	return info.Expires.After(until)
}

func (p *Provider) persistResolvedSession(ctx context.Context, acc *domain.Account, sec store.AccountSecret, info sessionInfo) error {
	if p.store == nil || info.AccessToken == "" {
		return nil
	}
	sec = chatGPTSessionOnlySnapshotSecret(sec)
	trimmed := strings.TrimSpace(sec.SessionToken)
	rawCookieWithoutRefresh := trimmed != "" && !strings.HasPrefix(trimmed, "{") && info.RefreshToken == ""

	changed := false
	persistedSession := false
	next := sec
	if !rawCookieWithoutRefresh && shouldPersistChatGPTSession(sec.SessionToken, info) {
		next.SessionToken = buildChatGPTSessionJSON(info)
		changed = true
		persistedSession = true
	}
	if info.RefreshToken != "" && next.RefreshToken != info.RefreshToken {
		next.RefreshToken = info.RefreshToken
		changed = true
	} else if info.RefreshToken == "" && persistedSession && next.RefreshToken != "" {
		next.RefreshToken = ""
		changed = true
	}
	if info.Email != "" && acc.Email == "" {
		acc.Email = info.Email
		changed = true
	}
	if info.PlanType != "" && acc.PlanTier == "" {
		acc.PlanTier = info.PlanType
		changed = true
	}
	if !changed {
		return nil
	}
	acc.UpdatedAt = time.Now()
	return p.store.UpsertAccount(ctx, acc, next)
}

func (p *Provider) persistCookieResolvedSession(ctx context.Context, acc *domain.Account, sec store.AccountSecret, info sessionInfo, setCookies []string, clearRefreshToken bool) error {
	if p.store == nil || info.AccessToken == "" {
		return nil
	}
	next := sec
	trimmed := strings.TrimSpace(sec.SessionToken)
	if trimmed != "" && !strings.HasPrefix(trimmed, "{") && len(next.Cookies) == 0 {
		next.Cookies = []byte("__Secure-next-auth.session-token=" + trimmed)
	}

	changed := false
	sessionJSON := buildChatGPTSessionJSON(info)
	if strings.TrimSpace(next.SessionToken) != sessionJSON {
		next.SessionToken = sessionJSON
		changed = true
	}
	if info.RefreshToken != "" {
		if next.RefreshToken != info.RefreshToken {
			next.RefreshToken = info.RefreshToken
			changed = true
		}
	} else if clearRefreshToken && next.RefreshToken != "" {
		next.RefreshToken = ""
		changed = true
	}
	if len(setCookies) > 0 {
		merged := chatGPTMergeSetCookieHeaders(next.Cookies, setCookies)
		if string(merged) != string(next.Cookies) {
			next.Cookies = merged
			changed = true
		}
	}
	if info.Email != "" && acc.Email == "" {
		acc.Email = info.Email
		changed = true
	}
	if info.PlanType != "" && acc.PlanTier == "" {
		acc.PlanTier = info.PlanType
		changed = true
	}
	if !changed {
		return nil
	}
	acc.UpdatedAt = time.Now()
	return p.store.UpsertAccount(ctx, acc, next)
}

func (p *Provider) persistPlanTier(ctx context.Context, acc *domain.Account, sec store.AccountSecret, tier string) error {
	if p.store == nil || tier == "" || acc.PlanTier == tier {
		return nil
	}
	acc.PlanTier = tier
	acc.UpdatedAt = time.Now()
	return p.store.UpsertAccount(ctx, acc, sec)
}

func (p *Provider) latestAccountSecretOr(ctx context.Context, accountID string, fallback store.AccountSecret) store.AccountSecret {
	if p.store == nil {
		return fallback
	}
	latest, err := p.store.GetAccountSecret(ctx, accountID)
	if err != nil {
		return fallback
	}
	return latest
}

func shouldPersistChatGPTSession(existing string, info sessionInfo) bool {
	trimmed := strings.TrimSpace(existing)
	if trimmed == "" {
		return true
	}
	if IsSessionOnlyAuthJSON(trimmed) {
		return false
	}
	if !strings.HasPrefix(trimmed, "{") {
		return info.RefreshToken != ""
	}
	old, err := parseSessionJSON([]byte(trimmed))
	if err != nil {
		return true
	}
	return old.AccessToken != info.AccessToken ||
		old.RefreshToken != info.RefreshToken ||
		old.SessionToken != info.SessionToken ||
		old.IDToken != info.IDToken ||
		!old.SessionExpires.Equal(info.SessionExpires) ||
		old.AccountID != info.AccountID ||
		old.PlanType != info.PlanType ||
		old.Email != info.Email ||
		old.ChatGPTUserID != info.ChatGPTUserID ||
		old.FedRAMP != info.FedRAMP ||
		old.Disabled != info.Disabled
}

func buildChatGPTSessionJSON(info sessionInfo) string {
	userID := firstNonEmpty(info.ChatGPTUserID, info.AccountID)
	expires := info.Expires
	if displayExpires := chatGPTSessionDisplayExpiry(info); !displayExpires.IsZero() {
		expires = displayExpires
	}
	out := map[string]any{
		"user": map[string]string{
			"id":    userID,
			"email": info.Email,
		},
		"expires": expires.Format(time.RFC3339),
		"account": map[string]any{
			"id":                         info.AccountID,
			"planType":                   info.PlanType,
			"isFedramp":                  info.FedRAMP,
			"is_fedramp":                 info.FedRAMP,
			"chatgpt_account_is_fedramp": info.FedRAMP,
			"chatgpt_account_id":         info.AccountID,
			"chatgpt_plan_type":          info.PlanType,
			"chatgpt_user_id":            info.ChatGPTUserID,
			"chatgptAccountIsFedramp":    info.FedRAMP,
			"chatgptAccountId":           info.AccountID,
			"chatgptPlanType":            info.PlanType,
			"chatgptUserId":              info.ChatGPTUserID,
		},
		"accessToken":                info.AccessToken,
		"refreshToken":               info.RefreshToken,
		"idToken":                    info.IDToken,
		"chatgpt_user_id":            info.ChatGPTUserID,
		"user_id":                    info.ChatGPTUserID,
		"chatgpt_account_id":         info.AccountID,
		"chatgpt_plan_type":          info.PlanType,
		"chatgpt_account_is_fedramp": info.FedRAMP,
		"chatgptAccountIsFedramp":    info.FedRAMP,
	}
	if info.SessionToken != "" {
		out["sessionToken"] = info.SessionToken
		out["session_token"] = info.SessionToken
	}
	if info.Disabled {
		out["disabled"] = true
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// NormalizeSessionOnlyAuthJSON converts a ChatGPT /api/auth/session response or
// an existing auth.json-like blob into a Codex/CPA-compatible JSON snapshot.
// This mode deliberately strips refresh_token material; callers should store no
// cookies with it so background refresh falls back to the fixed access token
// snapshot only.
func NormalizeSessionOnlyAuthJSON(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty session")
	}
	if !strings.HasPrefix(raw, "{") {
		return "", errors.New("session-only import requires /api/auth/session JSON or auth JSON")
	}
	info, err := parseSessionJSON([]byte(raw))
	if err != nil {
		return "", err
	}
	info.RefreshToken = ""
	return buildChatGPTSessionOnlyAuthJSON(info, time.Now().UTC()), nil
}

// IsSessionOnlyAuthJSON reports whether a stored ChatGPT JSON credential was
// imported as the CPA/session-only fixed access-token snapshot. These entries
// intentionally do not use OAuth refresh_token or web next-auth cookie recovery.
func IsSessionOnlyAuthJSON(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "{") {
		return false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return false
	}
	if mode, _ := obj["session_import_mode"].(string); strings.EqualFold(strings.TrimSpace(mode), "session_only_json") {
		return true
	}
	if synthetic, _ := obj["id_token_synthetic"].(bool); synthetic {
		return true
	}
	if tokens, ok := obj["tokens"].(map[string]any); ok {
		if synthetic, _ := tokens["id_token_synthetic"].(bool); synthetic {
			return true
		}
	}
	if strings.EqualFold(strings.TrimSpace(jsonStringValue(obj["type"])), "codex") &&
		chatGPTJSONAccessToken(obj) != "" &&
		chatGPTJSONRefreshToken(obj) == "" {
		return true
	}
	return false
}

func chatGPTJSONAccessToken(obj map[string]any) string {
	if value := firstNonEmpty(jsonStringValue(obj["access_token"]), jsonStringValue(obj["accessToken"])); value != "" {
		return value
	}
	for _, key := range []string{"token_data", "tokenData", "tokens", "metadata", "attributes", "token"} {
		if nested, ok := obj[key].(map[string]any); ok {
			if value := chatGPTJSONAccessToken(nested); value != "" {
				return value
			}
		}
	}
	return ""
}

func chatGPTJSONRefreshToken(obj map[string]any) string {
	if value := firstNonEmpty(jsonStringValue(obj["refresh_token"]), jsonStringValue(obj["refreshToken"])); value != "" {
		return value
	}
	for _, key := range []string{"token_data", "tokenData", "tokens", "metadata", "attributes", "token"} {
		if nested, ok := obj[key].(map[string]any); ok {
			if value := chatGPTJSONRefreshToken(nested); value != "" {
				return value
			}
		}
	}
	return ""
}

func jsonStringValue(v any) string {
	switch value := v.(type) {
	case string:
		return strings.TrimSpace(value)
	default:
		return ""
	}
}

func chatGPTSessionOnlySnapshotSecret(sec store.AccountSecret) store.AccountSecret {
	if !IsSessionOnlyAuthJSON(sec.SessionToken) {
		return sec
	}
	sec.RefreshToken = ""
	sec.Cookies = nil
	return sec
}

func chatGPTFixedAccessTokenSnapshot(sec store.AccountSecret) bool {
	if IsSessionOnlyAuthJSON(sec.SessionToken) {
		return true
	}
	if strings.TrimSpace(sec.RefreshToken) != "" || chatGPTRefreshTokenFromSession(sec.SessionToken) != "" {
		return false
	}
	if len(bytes.TrimSpace(sec.Cookies)) > 0 {
		return false
	}
	info, ok := parseStoredSessionSecret(sec.SessionToken, "")
	return ok && strings.TrimSpace(info.AccessToken) != "" && strings.TrimSpace(info.RefreshToken) == ""
}

func buildChatGPTSessionOnlyAuthJSON(info sessionInfo, now time.Time) string {
	userID := firstNonEmpty(info.ChatGPTUserID, info.AccountID)
	accountUserID := chatGPTAccountUserID(info.ChatGPTUserID, info.AccountID)
	idToken := info.IDToken
	syntheticIDToken := isSyntheticCodexIDToken(idToken)
	if idToken == "" && info.AccountID != "" {
		idToken = buildSyntheticCodexIDToken(info, now)
		syntheticIDToken = idToken != ""
	}
	expires := ""
	if displayExpires := chatGPTSessionDisplayExpiry(info); !displayExpires.IsZero() {
		expires = displayExpires.UTC().Format(time.RFC3339)
	}
	tokenData := map[string]any{
		"access_token":            info.AccessToken,
		"refresh_token":           "",
		"session_token":           info.SessionToken,
		"id_token":                idToken,
		"account_id":              info.AccountID,
		"chatgpt_account_id":      info.AccountID,
		"plan_type":               info.PlanType,
		"chatgpt_plan_type":       info.PlanType,
		"chatgpt_user_id":         info.ChatGPTUserID,
		"user_id":                 info.ChatGPTUserID,
		"chatgpt_account_user_id": accountUserID,
		"chatgptAccountId":        info.AccountID,
		"chatgptPlanType":         info.PlanType,
		"chatgptUserId":           info.ChatGPTUserID,
		"email":                   info.Email,
		"expires":                 expires,
		"expired":                 expires,
		"id_token_synthetic":      syntheticIDToken,
	}
	out := map[string]any{
		"auth_mode":    "chatgpt",
		"last_refresh": now.UTC().Format(time.RFC3339),
		"token_data":   tokenData,
		"tokens": map[string]any{
			"access_token":               info.AccessToken,
			"refresh_token":              "",
			"session_token":              info.SessionToken,
			"id_token":                   idToken,
			"account_id":                 info.AccountID,
			"chatgpt_account_id":         info.AccountID,
			"plan_type":                  info.PlanType,
			"chatgpt_plan_type":          info.PlanType,
			"chatgpt_user_id":            info.ChatGPTUserID,
			"user_id":                    info.ChatGPTUserID,
			"chatgpt_account_user_id":    accountUserID,
			"chatgpt_account_is_fedramp": info.FedRAMP,
			"chatgptAccountIsFedramp":    info.FedRAMP,
			"chatgptAccountId":           info.AccountID,
			"chatgptPlanType":            info.PlanType,
			"chatgptUserId":              info.ChatGPTUserID,
			"id_token_synthetic":         syntheticIDToken,
		},
		"user": map[string]string{
			"id":    userID,
			"email": info.Email,
		},
		"account": map[string]any{
			"id":                         info.AccountID,
			"planType":                   info.PlanType,
			"isFedramp":                  info.FedRAMP,
			"is_fedramp":                 info.FedRAMP,
			"chatgpt_account_is_fedramp": info.FedRAMP,
			"chatgpt_account_id":         info.AccountID,
			"chatgpt_plan_type":          info.PlanType,
			"chatgpt_user_id":            info.ChatGPTUserID,
			"chatgptAccountIsFedramp":    info.FedRAMP,
			"chatgptAccountId":           info.AccountID,
			"chatgptPlanType":            info.PlanType,
			"chatgptUserId":              info.ChatGPTUserID,
		},
		"expires":                    expires,
		"expired":                    expires,
		"email":                      info.Email,
		"type":                       "codex",
		"disabled":                   info.Disabled,
		"accessToken":                info.AccessToken,
		"access_token":               info.AccessToken,
		"refreshToken":               "",
		"refresh_token":              "",
		"sessionToken":               info.SessionToken,
		"session_token":              info.SessionToken,
		"idToken":                    idToken,
		"id_token":                   idToken,
		"account_id":                 info.AccountID,
		"accountId":                  info.AccountID,
		"chatgpt_account_id":         info.AccountID,
		"plan_type":                  info.PlanType,
		"planType":                   info.PlanType,
		"chatgpt_plan_type":          info.PlanType,
		"chatgpt_user_id":            info.ChatGPTUserID,
		"user_id":                    info.ChatGPTUserID,
		"chatgpt_account_user_id":    accountUserID,
		"chatgpt_account_is_fedramp": info.FedRAMP,
		"chatgptAccountIsFedramp":    info.FedRAMP,
		"id_token_synthetic":         syntheticIDToken,
		"session_import_mode":        "session_only_json",
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func chatGPTAccountUserID(userID, accountID string) string {
	userID = strings.TrimSpace(userID)
	accountID = strings.TrimSpace(accountID)
	if userID == "" || accountID == "" {
		return ""
	}
	return userID + "__" + accountID
}

func chatGPTSessionDisplayExpiry(info sessionInfo) time.Time {
	if !info.SessionExpires.IsZero() {
		return info.SessionExpires
	}
	return info.Expires
}

func isSyntheticCodexIDToken(token string) bool {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(addB64Padding(parts[0]))
		if err != nil {
			return false
		}
	}
	var header struct {
		CPASynthetic bool `json:"cpa_synthetic"`
	}
	return json.Unmarshal(payload, &header) == nil && header.CPASynthetic
}

func buildSyntheticCodexIDToken(info sessionInfo, now time.Time) string {
	if strings.TrimSpace(info.AccountID) == "" {
		return ""
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	exp := chatGPTSessionDisplayExpiry(info)
	if exp.IsZero() {
		exp = now.Add(90 * 24 * time.Hour)
	}
	authInfo := map[string]any{
		"chatgpt_account_id": info.AccountID,
	}
	if info.PlanType != "" {
		authInfo["chatgpt_plan_type"] = info.PlanType
	}
	if info.ChatGPTUserID != "" {
		authInfo["chatgpt_user_id"] = info.ChatGPTUserID
		authInfo["user_id"] = info.ChatGPTUserID
		if accountUserID := chatGPTAccountUserID(info.ChatGPTUserID, info.AccountID); accountUserID != "" {
			authInfo["chatgpt_account_user_id"] = accountUserID
		}
	}
	if info.FedRAMP {
		authInfo["chatgpt_account_is_fedramp"] = true
	}
	payload := map[string]any{
		"iat":                         now.Unix(),
		"exp":                         exp.Unix(),
		"https://api.openai.com/auth": authInfo,
	}
	if info.Email != "" {
		payload["email"] = info.Email
	}
	header := map[string]any{
		"alg":           "none",
		"typ":           "JWT",
		"cpa_synthetic": true,
	}
	return base64.RawURLEncoding.EncodeToString(mustJSON(header)) + "." +
		base64.RawURLEncoding.EncodeToString(mustJSON(payload)) + "."
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func (p *Provider) Invoke(ctx context.Context, acc *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	switch p.Mode {
	case ModeMock:
		return p.invokeMock(ctx, acc, req)
	case ModeReal:
		return p.invokeReal(ctx, acc, req)
	}
	return nil, fmt.Errorf("unknown chatgpt mode: %s", p.Mode)
}

func (p *Provider) Probe(ctx context.Context, acc *domain.Account) error {
	if p.Mode == ModeMock {
		return nil
	}
	if p.store == nil {
		return errors.New("store not wired")
	}
	sec, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		return fmt.Errorf("get secret: %w", err)
	}
	info, err := p.resolveSession(ctx, acc, sec)
	if err != nil {
		return err
	}
	_, err = p.fetchWhamUsageWithSecret(ctx, acc, info, sec)
	if isChatGPTRecoverableAuthError(err) && chatGPTCanRecoverAuthFailure(sec) {
		refreshed, refreshErr := p.forceRefreshSession(ctx, acc, sec)
		if refreshErr != nil {
			return fmt.Errorf("refresh after upstream auth failure: %w", refreshErr)
		}
		_, err = p.fetchWhamUsageWithSecret(ctx, acc, refreshed, p.latestAccountSecretOr(ctx, acc.ID, sec))
	}
	return err
}

func (p *Provider) Discover(ctx context.Context, acc *domain.Account) (*domain.QuotaState, error) {
	// Model lists sourced from CPA models.json (github.com/router-for-me/models).
	// Codex plan tiers: free/team share the same base set; plus/pro add spark model.
	codexBase := []domain.ModelCapability{
		{ID: "gpt-5.2", Available: true, ContextWindow: 128000, SupportsTools: true, SupportsVision: true},
		{ID: "gpt-5.3-codex", Available: true, ContextWindow: 128000, SupportsTools: true},
		{ID: "gpt-5.4", Available: true, ContextWindow: 128000, SupportsTools: true, SupportsVision: true},
		{ID: "gpt-5.4-mini", Available: true, ContextWindow: 128000, SupportsTools: true},
		{ID: "gpt-5.5", Available: true, ContextWindow: 128000, SupportsTools: true, SupportsVision: true},
		{ID: "gpt-image-2", Available: true, ContextWindow: 128000, SupportsVision: true},
		{ID: "codex-auto-review", Available: true, ContextWindow: 128000, SupportsTools: true},
	}
	codexSparkExtra := domain.ModelCapability{
		ID: "gpt-5.3-codex-spark", Available: true, ContextWindow: 128000, SupportsTools: true,
	}

	if p.Mode == ModeMock {
		models := append([]domain.ModelCapability{}, codexBase...)
		models = append(models, codexSparkExtra)
		return &domain.QuotaState{
			ShortWindow:      domain.QuotaWindow{Limit: 80, Used: 0, ResetAt: time.Now().Add(5 * time.Hour), Confidence: 1.0},
			LongWindow:       domain.QuotaWindow{Limit: 200, Used: 0, ResetAt: time.Now().Add(7 * 24 * time.Hour), Confidence: 1.0},
			DiscoveredModels: models,
			LastDiscoveryAt:  time.Now(),
		}, nil
	}
	if p.store == nil {
		return nil, errors.New("store not wired")
	}
	log.Printf("[chatgpt-discover] account=%s starting discover", acc.ID)
	sec, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		log.Printf("[chatgpt-discover] account=%s GetAccountSecret error: %v", acc.ID, err)
		return nil, err
	}
	log.Printf("[chatgpt-discover] account=%s secret loaded, session_token_len=%d, ua=%q", acc.ID, len(sec.SessionToken), acc.UA)
	info, err := p.resolveSession(ctx, acc, sec)
	if err != nil {
		log.Printf("[chatgpt-discover] account=%s Resolve error: %v", acc.ID, err)
		// Detect ban from session error
		errMsg := strings.ToLower(err.Error())
		if strings.Contains(errMsg, "banned") || strings.Contains(errMsg, "deactivated") || strings.Contains(errMsg, "suspended") {
			acc.State = domain.StateBanned
			log.Printf("[chatgpt-discover] account=%s BANNED detected during session resolve", acc.ID)
		}
		return nil, err
	}
	log.Printf("[chatgpt-discover] account=%s resolved: access_token_len=%d, account_id=%q, plan=%q, expires=%v",
		acc.ID, len(info.AccessToken), info.AccountID, info.PlanType, info.Expires)

	quotaState, err := p.fetchConversationLimitWithSecret(ctx, acc, info, sec)
	if isChatGPTRecoverableAuthError(err) && chatGPTCanRecoverAuthFailure(sec) {
		log.Printf("[chatgpt-discover] account=%s upstream auth failed, refreshing session and retrying quota fetch", acc.ID)
		refreshed, refreshErr := p.forceRefreshSession(ctx, acc, sec)
		if refreshErr != nil {
			return nil, fmt.Errorf("refresh after upstream auth failure: %w", refreshErr)
		}
		info = refreshed
		quotaState, err = p.fetchConversationLimitWithSecret(ctx, acc, info, p.latestAccountSecretOr(ctx, acc.ID, sec))
	}
	if err != nil {
		return nil, err
	}

	tier := quotaState.PlanTier
	if tier == "" && info.PlanType != "" {
		tier = info.PlanType
	}
	if tier == "" {
		tier = acc.PlanTier
	}
	models := append([]domain.ModelCapability{}, codexBase...)
	if tier == "plus" || tier == "pro" || tier == "team" {
		models = append(models, codexSparkExtra)
	}
	quotaState.DiscoveredModels = models
	quotaState.PlanTier = tier
	quotaState.LastDiscoveryAt = time.Now()
	if err := p.persistPlanTier(ctx, acc, sec, tier); err != nil {
		log.Printf("[chatgpt-discover] account=%s persist plan tier: %v", acc.ID, err)
	}
	log.Printf("[chatgpt-discover] account=%s discover done: tier=%s 5h=%.1f/%.1f(conf=%.1f) 7d=%.1f/%.1f(conf=%.1f) models=%d",
		acc.ID, tier,
		quotaState.ShortWindow.Used, quotaState.ShortWindow.Limit, quotaState.ShortWindow.Confidence,
		quotaState.LongWindow.Used, quotaState.LongWindow.Limit, quotaState.LongWindow.Confidence,
		len(quotaState.DiscoveredModels))
	return quotaState, nil
}

// fetchConversationLimit tries two endpoints to get real quota:
//  1. /backend-api/wham/usage (Codex CLI style, current)
//  2. /backend-api/conversation_limit (ChatGPT web style, legacy fallback)
func (p *Provider) fetchConversationLimit(ctx context.Context, acc *domain.Account, info sessionInfo) (*domain.QuotaState, error) {
	return p.fetchConversationLimitWithSecret(ctx, acc, info, store.AccountSecret{})
}

func (p *Provider) fetchConversationLimitWithSecret(ctx context.Context, acc *domain.Account, info sessionInfo, sec store.AccountSecret) (*domain.QuotaState, error) {
	state := &domain.QuotaState{
		ShortWindow: domain.QuotaWindow{ResetAt: time.Now().Add(5 * time.Hour), Confidence: 0.5},
		LongWindow:  domain.QuotaWindow{ResetAt: time.Now().Add(7 * 24 * time.Hour), Confidence: 0.5},
	}

	// Try wham/usage first (Codex CLI endpoint)
	whamState, err := p.fetchWhamUsageWithSecret(ctx, acc, info, sec)
	if err == nil {
		*state = *whamState
		log.Printf("[chatgpt-quota] wham/usage ok: 5h=%.0f/%.0f 7d=%.0f/%.0f",
			state.ShortWindow.Used, state.ShortWindow.Limit,
			state.LongWindow.Used, state.LongWindow.Limit)
		return state, nil
	}
	if isChatGPTRecoverableAuthError(err) {
		return nil, err
	}
	if isBannedError(err) {
		return nil, err
	}
	log.Printf("[chatgpt-quota] wham/usage failed, trying conversation_limit: %v", err)
	// Fallback to conversation_limit (legacy ChatGPT web endpoint)
	if err := p.fetchLegacyConversationLimitWithSecret(ctx, state, acc, info, sec); err != nil {
		return nil, err
	}
	log.Printf("[chatgpt-quota] conversation_limit: 5h=%.0f/%.0f 7d=%.0f/%.0f",
		state.ShortWindow.Used, state.ShortWindow.Limit,
		state.LongWindow.Used, state.LongWindow.Limit)
	return state, nil
}

// fetchWhamUsage calls /backend-api/wham/usage (Codex CLI's rate limit endpoint).
// Response format:
//
//	{"rate_limit":{"used_percent":0.25,"window_minutes":300,"resets_at":1714...},
//	 "additional_rate_limits":[{"limit_name":"weekly","rate_limit":{"used_percent":0.1,"window_minutes":10080,"resets_at":...}}],
//	 "credits":{"has_credits":true,"unlimited":false,"balance":"$4.20"},
//	 "plan_type":"plus"}
func (p *Provider) fetchWhamUsage(ctx context.Context, acc *domain.Account, info sessionInfo) (*domain.QuotaState, error) {
	return p.fetchWhamUsageWithSecret(ctx, acc, info, store.AccountSecret{})
}

func (p *Provider) fetchWhamUsageWithSecret(ctx context.Context, acc *domain.Account, info sessionInfo, sec store.AccountSecret) (*domain.QuotaState, error) {
	state := &domain.QuotaState{
		ShortWindow: domain.QuotaWindow{ResetAt: time.Now().Add(5 * time.Hour), Confidence: 0.5},
		LongWindow:  domain.QuotaWindow{ResetAt: time.Now().Add(7 * 24 * time.Hour), Confidence: 0.5},
		TierWindows: make(map[string]domain.QuotaWindow),
	}
	ua, originator := codexHeaderProfile(acc, sec)
	log.Printf("[chatgpt-quota] wham/usage: starting, account_id=%q, access_token_len=%d", info.AccountID, len(info.AccessToken))
	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://chatgpt.com/backend-api/wham/usage", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+info.AccessToken)
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Originator", originator)
	setChatGPTAccountHeaders(req.Header, info)
	setChatGPTCookieHeader(req.Header, sec)

	client, err := p.httpClientForAccount(ctx, acc)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wham/usage request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != 200 {
		if schedulerClassifyBanned(resp.StatusCode, body) {
			return nil, fmt.Errorf("account banned: wham/usage %d: %s", resp.StatusCode, snippet(body))
		}
		return nil, fmt.Errorf("wham/usage %d: %s", resp.StatusCode, snippet(body))
	}
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("wham/usage invalid json: %s", snippet(body))
	}
	parseWhamUsage(body, state)
	if state.ShortWindow.Confidence <= 0.5 && state.LongWindow.Confidence <= 0.5 {
		return nil, fmt.Errorf("wham/usage missing quota windows")
	}
	log.Printf("[chatgpt-quota] wham/usage: final short={used=%.1f lim=%.1f conf=%.1f} long={used=%.1f lim=%.1f conf=%.1f} plan=%s tiers=%d",
		state.ShortWindow.Used, state.ShortWindow.Limit, state.ShortWindow.Confidence,
		state.LongWindow.Used, state.LongWindow.Limit, state.LongWindow.Confidence,
		state.PlanTier, len(state.TierWindows))
	return state, nil
}

func parseWhamUsage(body []byte, state *domain.QuotaState) {
	root := gjson.ParseBytes(body)

	// Parse primary window (5h)
	primary := root.Get("rate_limit.primary_window")
	primaryWindow, primaryWinSec, primaryOK := parseWhamWindow(primary, 5*time.Hour)
	if primaryOK {
		state.ShortWindow = primaryWindow
	} else if legacy := root.Get("rate_limit"); legacy.Get("used_percent").Exists() {
		legacyWindow, legacyWinSec, ok := parseWhamWindow(legacy, 5*time.Hour)
		if ok {
			state.ShortWindow = legacyWindow
			primaryWinSec = legacyWinSec
			primaryOK = true
		}
	}

	// Parse secondary window (7d)
	secondary := root.Get("rate_limit.secondary_window")
	secondaryWindow, secondaryWinSec, secondaryOK := parseWhamWindow(secondary, 7*24*time.Hour)
	if secondaryOK {
		state.LongWindow = secondaryWindow
	}

	// Store plan type
	planType := root.Get("plan_type").String()
	if planType != "" {
		state.PlanTier = planType
	}

	// Populate TierWindows with dynamic names based on window duration.
	if state.TierWindows == nil {
		state.TierWindows = make(map[string]domain.QuotaWindow)
	}
	if primaryOK {
		state.TierWindows[windowSecsToTierName(primaryWinSec)] = state.ShortWindow
	}
	if secondaryOK {
		state.TierWindows[windowSecsToTierName(secondaryWinSec)] = state.LongWindow
	}

	// Parse additional_rate_limits when it is either an array or an object.
	root.Get("additional_rate_limits").ForEach(func(k, v gjson.Result) bool {
		parseWhamExtraRateLimit(k.String(), v, state)
		return true
	})

	// Codex-Manager also records arbitrary *_rate_limit siblings. Preserve them
	// as named tier windows so model-specific or feature-specific caps are visible.
	root.ForEach(func(k, v gjson.Result) bool {
		name := k.String()
		if name != "rate_limit" && strings.HasSuffix(name, "_rate_limit") {
			parseWhamExtraRateLimit(name, v, state)
		}
		return true
	})

	// Parse credits field.
	if credits := root.Get("credits"); credits.Exists() {
		state.ExtraUsage = &domain.ExtraUsage{
			IsEnabled: credits.Get("has_credits").Bool(),
			Currency:  credits.Get("balance").String(),
		}
	}
}

func parseWhamExtraRateLimit(sourceName string, raw gjson.Result, state *domain.QuotaState) {
	if !raw.Exists() {
		return
	}
	if state.TierWindows == nil {
		state.TierWindows = make(map[string]domain.QuotaWindow)
	}
	baseName := whamRateLimitName(sourceName, raw)
	rl := raw.Get("rate_limit")
	if !rl.Exists() {
		rl = raw
	}

	primary := rl.Get("primary_window")
	secondary := rl.Get("secondary_window")
	parsedNested := false
	if primary.Exists() {
		if window, winSec, ok := parseWhamWindow(primary, 0); ok {
			name := whamWindowTierName(baseName, "primary", winSec)
			state.TierWindows[name] = window
			parsedNested = true
		}
	}
	if secondary.Exists() {
		if window, winSec, ok := parseWhamWindow(secondary, 0); ok {
			name := whamWindowTierName(baseName, "secondary", winSec)
			state.TierWindows[name] = window
			parsedNested = true
		}
	}
	if parsedNested {
		return
	}

	window, winSec, ok := parseWhamWindow(rl, 0)
	if !ok {
		return
	}
	name := firstNonEmpty(baseName, windowSecsToTierName(winSec))
	if name == "" {
		name = "unknown"
	}
	state.TierWindows[name] = window
}

func whamWindowTierName(baseName, suffix string, winSec int64) string {
	baseName = strings.TrimSpace(baseName)
	if baseName == "" {
		return windowSecsToTierName(winSec)
	}
	return baseName + "_" + suffix
}

func whamRateLimitName(sourceName string, raw gjson.Result) string {
	sourceName = strings.TrimSpace(sourceName)
	if sourceName == "" || isNumericString(sourceName) {
		sourceName = ""
	}
	sourceName = strings.TrimSuffix(sourceName, "_rate_limit")
	return firstNonEmpty(
		raw.Get("limit_name").String(),
		raw.Get("metered_feature").String(),
		raw.Get("limit_id").String(),
		sourceName,
	)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func normalizeChatGPTAccountUserID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if userID, _, ok := strings.Cut(value, "__"); ok {
		return strings.TrimSpace(userID)
	}
	return value
}

func isNumericString(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func parseWhamWindow(raw gjson.Result, defaultWindow time.Duration) (domain.QuotaWindow, int64, bool) {
	if !raw.Exists() || !raw.Get("used_percent").Exists() {
		return domain.QuotaWindow{}, 0, false
	}
	usedPct := raw.Get("used_percent").Float()
	winSec := raw.Get("limit_window_seconds").Int()
	if winSec <= 0 {
		if mins := raw.Get("window_minutes").Int(); mins > 0 {
			winSec = mins * 60
		}
	}
	resetAt := raw.Get("reset_at").Int()
	if resetAt <= 0 {
		resetAt = raw.Get("resets_at").Int()
	}
	var resetTime time.Time
	if resetAt > 0 {
		resetTime = time.Unix(resetAt, 0)
	} else if after := raw.Get("reset_after_seconds").Int(); after > 0 {
		resetTime = time.Now().Add(time.Duration(after) * time.Second)
	} else if defaultWindow > 0 {
		resetTime = time.Now().Add(defaultWindow)
	}
	if winSec <= 0 && defaultWindow > 0 {
		winSec = int64(defaultWindow / time.Second)
	}
	return domain.QuotaWindow{
		Limit:      100,
		Used:       usedPct,
		ResetAt:    resetTime,
		Confidence: 1.0,
	}, winSec, true
}

func windowSecsToTierName(secs int64) string {
	switch secs {
	case 18000:
		return "five_hour"
	case 604800:
		return "seven_day"
	default:
		hours := secs / 3600
		if hours >= 24 {
			return fmt.Sprintf("%d_day", hours/24)
		}
		if hours > 0 {
			return fmt.Sprintf("%d_hour", hours)
		}
		return "unknown"
	}
}

// fetchLegacyConversationLimit calls /backend-api/conversation_limit (legacy).
func (p *Provider) fetchLegacyConversationLimit(ctx context.Context, state *domain.QuotaState, acc *domain.Account, info sessionInfo) error {
	return p.fetchLegacyConversationLimitWithSecret(ctx, state, acc, info, store.AccountSecret{})
}

func (p *Provider) fetchLegacyConversationLimitWithSecret(ctx context.Context, state *domain.QuotaState, acc *domain.Account, info sessionInfo, sec store.AccountSecret) error {
	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://chatgpt.com/backend-api/conversation_limit", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+info.AccessToken)
	ua, originator := codexHeaderProfile(acc, sec)
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Originator", originator)
	setChatGPTAccountHeaders(req.Header, info)
	setChatGPTCookieHeader(req.Header, sec)

	client, err := p.httpClientForAccount(ctx, acc)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != 200 {
		if schedulerClassifyBanned(resp.StatusCode, body) {
			return fmt.Errorf("account banned: conversation_limit %d: %s", resp.StatusCode, snippet(body))
		}
		return fmt.Errorf("conversation_limit %d: %s", resp.StatusCode, snippet(body))
	}

	parse := func(key string) (limit, remaining int64, resetAt time.Time) {
		limit = gjson.GetBytes(body, key+".limit").Int()
		remaining = gjson.GetBytes(body, key+".remaining").Int()
		if rt := gjson.GetBytes(body, key+".reset_time_utc").String(); rt != "" {
			resetAt, _ = time.Parse(time.RFC3339, rt)
		}
		if resetAt.IsZero() {
			resetAt = time.Now().Add(5 * time.Hour)
		}
		return
	}

	shortLimit, shortRem, shortReset := parse("message_cap_ffp")
	longLimit, longRem, longReset := parse("message_cap_ffp_7d")

	if shortLimit > 0 {
		state.ShortWindow = domain.QuotaWindow{
			Limit:      float64(shortLimit),
			Used:       float64(shortLimit - shortRem),
			ResetAt:    shortReset,
			Confidence: 1.0,
		}
	}
	if longLimit > 0 {
		state.LongWindow = domain.QuotaWindow{
			Limit:      float64(longLimit),
			Used:       float64(longLimit - longRem),
			ResetAt:    longReset,
			Confidence: 1.0,
		}
	}
	return nil
}

func setChatGPTAccountHeaders(h http.Header, info sessionInfo) {
	if info.AccountID != "" {
		h["ChatGPT-Account-ID"] = []string{info.AccountID}
	}
	if info.FedRAMP {
		h["X-OpenAI-Fedramp"] = []string{"true"}
	}
}

func setChatGPTCookieHeader(h http.Header, sec store.AccountSecret) {
	if cookieHeader := chatGPTCookieHeaderFromSecret(sec); cookieHeader != "" {
		h.Set("Cookie", cookieHeader)
	}
}

func codexUA(acc *domain.Account) string {
	ua, _ := codexHeaderProfile(acc, store.AccountSecret{})
	return ua
}

func webSessionUA(acc *domain.Account) string {
	if acc != nil && strings.TrimSpace(acc.UA) != "" {
		ua := strings.TrimSpace(acc.UA)
		if !looksLikeCodexUserAgent(ua) && looksLikeBrowserUserAgent(ua) {
			return ua
		}
	}
	return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
}

func codexHeaderProfile(acc *domain.Account, sec store.AccountSecret) (ua, originator string) {
	if acc != nil && acc.UA != "" {
		ua = acc.UA
	}
	if IsSessionOnlyAuthJSON(sec.SessionToken) {
		if ua == "" || !looksLikeCodexUserAgent(ua) {
			ua = cpaCodexUserAgent
		}
		return ua, cpaCodexOriginator
	}
	if ua == "" {
		ua = defaultCodexUserAgent
	}
	return ua, defaultCodexOriginator
}

func looksLikeCodexUserAgent(ua string) bool {
	ua = strings.ToLower(strings.TrimSpace(ua))
	return strings.Contains(ua, "codex")
}

func looksLikeBrowserUserAgent(ua string) bool {
	ua = strings.ToLower(strings.TrimSpace(ua))
	return strings.Contains(ua, "mozilla/") &&
		(strings.Contains(ua, "chrome/") ||
			strings.Contains(ua, "firefox/") ||
			strings.Contains(ua, "safari/") ||
			strings.Contains(ua, "edg/"))
}

func schedulerClassifyBanned(status int, body []byte) bool {
	return scheduler.ClassifyError(status, string(body), nil) == domain.ErrBanned
}

func isBannedError(err error) bool {
	return scheduler.ClassifyError(0, "", err) == domain.ErrBanned
}

func (p *Provider) invokeMock(ctx context.Context, acc *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	out := make(chan ir.Event, 16)
	go func() {
		defer close(out)
		var lastUserText string
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == ir.RoleUser {
				for _, pp := range req.Messages[i].Parts {
					if pp.Kind == ir.PartText {
						lastUserText = pp.Text
						break
					}
				}
				break
			}
		}
		preview := strings.TrimSpace(lastUserText)
		if len(preview) > 60 {
			preview = preview[:60] + "..."
		}
		response := fmt.Sprintf(
			"[mock chatgpt provider · account=%s · model=%s]\n\nYou said: %q\n\nThis is a deterministic mock response from the LLM-pool gateway. Replace with real ChatGPT integration when subscription credentials are captured.",
			acc.ID, req.Model, preview,
		)
		words := strings.Fields(response)
		for _, w := range words {
			select {
			case <-ctx.Done():
				out <- ir.Event{Kind: ir.EvError, Err: ctx.Err()}
				return
			case out <- ir.Event{Kind: ir.EvTextDelta, Text: w + " "}:
			}
			time.Sleep(15 * time.Millisecond)
		}
		out <- ir.Event{Kind: ir.EvUsage, InputTokens: 50, OutputTokens: len(words)}
		out <- ir.Event{Kind: ir.EvDone, FinishReason: "stop"}
	}()
	return out, nil
}

// ----- Real mode (Codex Responses API) -----
//
// We POST to https://chatgpt.com/backend-api/codex/responses, which:
//   - consumes the *Codex* quota bucket (separate from regular ChatGPT chat
//     limits) — primary 5h window, secondary 7d window, both reported in
//     X-Codex-* response headers
//   - accepts the OpenAI Responses API request shape (input array, instructions,
//     reasoning, tools, ...) and streams response.* SSE events back
//   - bypasses the chatgpt.com web Turnstile / sentinel layer entirely
//
// The Plus-account-allowed model slugs come from
// /backend-api/codex/models?client_version=0.45.0 (e.g. "gpt-5.2").

func (p *Provider) invokeReal(ctx context.Context, acc *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	if p.store == nil {
		return nil, errors.New("chatgpt: store not wired")
	}
	sec, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		return nil, fmt.Errorf("get secret: %w", err)
	}
	info, err := p.resolveSession(ctx, acc, sec)
	if err != nil {
		return nil, fmt.Errorf("resolve session: %w", err)
	}

	model := mapToCodexModelSlug(req.Model)
	body, err := buildResponsesBody(req, model)
	if err != nil {
		return nil, err
	}

	resp, err := p.doCodexResponsesWithSecret(ctx, acc, info, sec, body)
	if err != nil {
		return nil, fmt.Errorf("post codex/responses: %w", err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if isChatGPTRecoverableAuthResponse(resp.StatusCode, b) {
			if chatGPTCanRecoverAuthFailure(sec) {
				refreshed, refreshErr := p.forceRefreshSession(ctx, acc, sec)
				if refreshErr != nil {
					return nil, fmt.Errorf("refresh after upstream auth failure: %w", refreshErr)
				}
				resp, err = p.doCodexResponsesWithSecret(ctx, acc, refreshed, p.latestAccountSecretOr(ctx, acc.ID, sec), body)
				if err != nil {
					return nil, fmt.Errorf("post codex/responses after refresh: %w", err)
				}
				if resp.StatusCode == 200 {
					out := make(chan ir.Event, 32)
					go streamResponsesSSE(ctx, resp.Body, out)
					return out, nil
				}
				b, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
		}
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, snippet(b))
	}

	out := make(chan ir.Event, 32)
	go streamResponsesSSE(ctx, resp.Body, out)
	return out, nil
}

func (p *Provider) doCodexResponses(ctx context.Context, acc *domain.Account, info sessionInfo, body []byte) (*http.Response, error) {
	return p.doCodexResponsesWithSecret(ctx, acc, info, store.AccountSecret{}, body)
}

func (p *Provider) doCodexResponsesWithSecret(ctx context.Context, acc *domain.Account, info sessionInfo, sec store.AccountSecret, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		"https://chatgpt.com/backend-api/codex/responses",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	ua, originator := codexHeaderProfile(acc, sec)
	httpReq.Header.Set("Authorization", "Bearer "+info.AccessToken)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("User-Agent", ua)
	httpReq.Header["OpenAI-Beta"] = []string{"responses=experimental"}
	httpReq.Header.Set("Originator", originator)
	setChatGPTAccountHeaders(httpReq.Header, info)
	setChatGPTCookieHeader(httpReq.Header, sec)
	setCodexSessionHeaders(httpReq.Header, body)
	client, err := p.httpClientForAccount(ctx, acc)
	if err != nil {
		return nil, err
	}
	return client.Do(httpReq)
}

func (p *Provider) invokeWebConversationWithInfo(ctx context.Context, acc *domain.Account, req *ir.Request, info sessionInfo, sec store.AccountSecret) (<-chan ir.Event, error) {
	body, err := buildConversationBody(req)
	if err != nil {
		return nil, err
	}
	resp, err := p.doWebConversationWithSecret(ctx, acc, info, sec, body)
	if err != nil {
		return nil, fmt.Errorf("post web conversation: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
		resp.Body.Close()
		return nil, fmt.Errorf("web conversation %d: %s", resp.StatusCode, snippet(b))
	}
	out := make(chan ir.Event, 32)
	go streamSSE(ctx, resp.Body, out, req.Model)
	return out, nil
}

func (p *Provider) doWebConversationWithSecret(ctx context.Context, acc *domain.Account, info sessionInfo, sec store.AccountSecret, body []byte) (*http.Response, error) {
	ua := webSessionUA(acc)
	cookieHeader := chatGPTCookieHeaderFromSecret(sec)
	requirementsToken, proofToken, err := p.fetchChatRequirements(ctx, acc, info.AccessToken, ua, cookieHeader)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		"https://chatgpt.com/backend-api/conversation",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+info.AccessToken)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("User-Agent", ua)
	httpReq.Header.Set("Origin", "https://chatgpt.com")
	httpReq.Header.Set("Referer", "https://chatgpt.com/")
	httpReq.Header.Set("OAI-Language", "en-US")
	if requirementsToken != "" {
		httpReq.Header.Set("OpenAI-Sentinel-Chat-Requirements-Token", requirementsToken)
	}
	if proofToken != "" {
		httpReq.Header.Set("OpenAI-Sentinel-Proof-Token", proofToken)
	}
	if cookieHeader != "" {
		httpReq.Header.Set("Cookie", cookieHeader)
	}
	client, err := p.httpClientForAccount(ctx, acc)
	if err != nil {
		return nil, err
	}
	return client.Do(httpReq)
}

// InvokeRaw sends a raw Responses API JSON body to the upstream Codex endpoint
// and returns the raw SSE response stream for passthrough proxying.
func (p *Provider) InvokeRaw(ctx interface{}, accountID string, body []byte) (io.ReadCloser, int, error) {
	rctx, ok := ctx.(context.Context)
	if !ok {
		return nil, 0, errors.New("invalid context")
	}
	if p.store == nil {
		return nil, 0, errors.New("chatgpt: store not wired")
	}
	sec, err := p.store.GetAccountSecret(rctx, accountID)
	if err != nil {
		return nil, 0, fmt.Errorf("get secret: %w", err)
	}
	acc, err := p.store.GetAccount(rctx, accountID)
	if err != nil {
		return nil, 0, fmt.Errorf("get account: %w", err)
	}
	info, err := p.resolveSession(rctx, acc, sec)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve session: %w", err)
	}

	body = normalizeCodexRawResponsesBody(body)

	resp, err := p.doCodexResponsesWithSecret(rctx, acc, info, sec, body)
	if err != nil {
		return nil, 0, fmt.Errorf("post codex/responses: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
		resp.Body.Close()
		if isChatGPTRecoverableAuthResponse(resp.StatusCode, respBody) {
			if chatGPTCanRecoverAuthFailure(sec) {
				refreshed, refreshErr := p.forceRefreshSession(rctx, acc, sec)
				if refreshErr != nil {
					return nil, 0, fmt.Errorf("refresh after upstream auth failure: %w", refreshErr)
				}
				resp, err = p.doCodexResponsesWithSecret(rctx, acc, refreshed, p.latestAccountSecretOr(rctx, acc.ID, sec), body)
				if err != nil {
					return nil, 0, fmt.Errorf("post codex/responses after refresh: %w", err)
				}
				return resp.Body, resp.StatusCode, nil
			}
		}
		return io.NopCloser(bytes.NewReader(respBody)), resp.StatusCode, nil
	}
	return resp.Body, resp.StatusCode, nil
}

// buildResponsesBody renders an IR request into the Responses API JSON shape.
// The conversation history is passed as the `input` array; each turn becomes
// a {"type":"message","role":"...","content":[{"type":"input_text","text":...}]} entry.
// buildResponsesBody converts an IR request to the OpenAI Responses API format
// used by chatgpt.com/backend-api/codex/responses. Includes full tool support
// so Claude Code plan mode / multi-agent / tool_use works via GPT backend.
func buildResponsesBody(req *ir.Request, model string) ([]byte, error) {
	// --- input items ---
	var items []any
	for _, m := range req.Messages {
		switch m.Role {
		case ir.RoleSystem:
			continue // injected into instructions below
		case ir.RoleUser:
			// Flush tool results first (they reference the preceding function_calls),
			// then append any text content as a user message.
			var textParts []any
			for _, p := range m.Parts {
				switch p.Kind {
				case ir.PartText:
					textParts = append(textParts, map[string]any{"type": "input_text", "text": p.Text})
				case ir.PartImage:
					url := p.ImageURL
					if url == "" && len(p.ImageBytes) > 0 {
						url = "data:" + p.ImageMedia + ";base64," + string(p.ImageBytes)
					}
					if url != "" {
						textParts = append(textParts, map[string]any{"type": "input_image", "image_url": url})
					}
				case ir.PartToolResult:
					var content string
					if len(p.ToolResultBytes) > 0 {
						content = string(p.ToolResultBytes)
					}
					items = append(items, map[string]any{
						"type":    "function_call_output",
						"call_id": p.ToolResultID,
						"output":  content,
					})
				}
			}
			if len(textParts) > 0 {
				items = append(items, map[string]any{"type": "message", "role": "user", "content": textParts})
			}
		case ir.RoleAssistant:
			// Maintain correct interleaving: text before tool calls.
			// Flush accumulated text parts before each function_call so GPT
			// sees the assistant's reasoning context before the tool invocation.
			var textParts []any
			flushText := func() {
				if len(textParts) > 0 {
					items = append(items, map[string]any{"type": "message", "role": "assistant", "content": textParts})
					textParts = nil
				}
			}
			for _, p := range m.Parts {
				switch p.Kind {
				case ir.PartText:
					textParts = append(textParts, map[string]any{"type": "output_text", "text": p.Text})
				case ir.PartToolUse:
					flushText()
					items = append(items, map[string]any{
						"type":      "function_call",
						"call_id":   p.ToolUseID,
						"name":      p.ToolUseName,
						"arguments": string(p.ToolUseInput),
					})
				}
			}
			flushText()
		default:
			for _, p := range m.Parts {
				if p.Kind == ir.PartText && p.Text != "" {
					items = append(items, map[string]any{
						"type": "message", "role": string(m.Role),
						"content": []any{map[string]any{"type": "input_text", "text": p.Text}},
					})
				}
			}
		}
	}

	// --- instructions (system prompt) ---
	instructions := req.System
	if instructions == "" {
		instructions = "You are a helpful assistant."
	}

	// --- tools (function definitions) ---
	var tools []any
	for _, t := range req.Tools {
		var schema any
		_ = json.Unmarshal(t.Schema, &schema)
		tools = append(tools, map[string]any{
			"type":        "function",
			"name":        t.Name,
			"description": t.Description,
			"parameters":  schema,
			"strict":      false,
		})
	}

	body := map[string]any{
		"model":               model,
		"input":               items,
		"instructions":        instructions,
		"reasoning":           map[string]string{"effort": pickReasoningEffort(req)},
		"store":               false,
		"stream":              true,
		"include":             []string{"reasoning.encrypted_content"},
		"parallel_tool_calls": true,
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if tier := normalizeCodexServiceTier(req.ServiceTier); tier != "" {
		body["service_tier"] = tier
	}
	// Codex Responses API does NOT support max_output_tokens — omit.
	return json.Marshal(body)
}

// mapToCodexModelSlug converts incoming model ids (gpt-4o, gpt-4o-mini, ...)
// into the slug ChatGPT-Codex actually accepts for Plus/Pro accounts.
// The accepted slugs come from /backend-api/codex/models — at time of writing
// "gpt-5.2" is the canonical default for Plus.
func mapToCodexModelSlug(m string) string {
	switch m {
	case "":
		return "gpt-5.2"
	case "gpt-5.2", "gpt-5.3", "gpt-5":
		return m
	case "gpt-4o", "gpt-4o-mini", "gpt-4.1", "gpt-4-turbo", "gpt-4":
		// these aren't supported on the codex/responses path for ChatGPT
		// accounts; transparently upgrade to the default Codex model.
		return "gpt-5.2"
	case "o1", "o1-mini", "o3", "o3-mini", "o4-mini":
		return "gpt-5.2"
	}
	return m
}

// pickReasoningEffort maps the IR reasoning effort to the Codex Responses API
// "effort" field. Codex supports: low | medium | high | xhigh (highest).
func pickReasoningEffort(req *ir.Request) string {
	switch strings.ToLower(strings.TrimSpace(req.ReasoningEffort)) {
	case "low", "minimal", "none":
		return "low"
	case "medium", "normal":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "max", "maximum":
		return "xhigh"
	case "auto", "":
		return "xhigh"
	}
	return "xhigh"
}

func normalizeCodexServiceTier(tier string) string {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "priority", "fast":
		return "priority"
	default:
		return ""
	}
}

func normalizeCodexRawResponsesBody(body []byte) []byte {
	tier := gjson.GetBytes(body, "service_tier")
	if !tier.Exists() || tier.Type != gjson.String {
		return body
	}
	normalized := normalizeCodexServiceTier(tier.String())
	if normalized == "" {
		if strings.EqualFold(strings.TrimSpace(tier.String()), "default") {
			next, err := sjson.DeleteBytes(body, "service_tier")
			if err == nil {
				return next
			}
		}
		return body
	}
	if normalized == tier.String() {
		return body
	}
	next, err := sjson.SetBytes(body, "service_tier", normalized)
	if err != nil {
		return body
	}
	return next
}

// buildConversationBody renders an IR request into the JSON ChatGPT expects.
func buildConversationBody(req *ir.Request) ([]byte, error) {
	type msgContent struct {
		ContentType string   `json:"content_type"`
		Parts       []string `json:"parts"`
	}
	type msgAuthor struct {
		Role string `json:"role"`
	}
	type message struct {
		ID       string     `json:"id"`
		Author   msgAuthor  `json:"author"`
		Content  msgContent `json:"content"`
		Metadata struct {
			SerializationMetadata struct {
				CustomSymbolOffsets []any `json:"custom_symbol_offsets"`
			} `json:"serialization_metadata"`
		} `json:"metadata"`
	}
	model := req.Model
	if model == "" {
		model = "gpt-4o"
	}
	chatgptModel := mapToChatgptModelSlug(model)

	msgs := []message{}
	// We forward only the most recent user message; ChatGPT keeps history
	// server-side via parent_message_id when we use the same conversation,
	// but for the MVP we send the current turn and let the model see the
	// full inline history through the system context that the encoder built.
	// Actually ChatGPT expects ONE user message in the body — history must
	// be replayed via the conversation_id. To keep MVP simple, we collapse
	// the full conversation into the single user turn.
	var collapsed strings.Builder
	if req.System != "" {
		collapsed.WriteString(req.System)
		collapsed.WriteString("\n\n")
	}
	for i, m := range req.Messages {
		if i == len(req.Messages)-1 && m.Role == ir.RoleUser {
			break
		}
		var role string
		switch m.Role {
		case ir.RoleUser:
			role = "User"
		case ir.RoleAssistant:
			role = "Assistant"
		default:
			role = string(m.Role)
		}
		for _, p := range m.Parts {
			if p.Kind == ir.PartText && p.Text != "" {
				collapsed.WriteString(role + ": " + p.Text + "\n")
			}
		}
	}
	// Last user message (or whatever the final message is)
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("empty messages")
	}
	last := req.Messages[len(req.Messages)-1]
	for _, p := range last.Parts {
		if p.Kind == ir.PartText && p.Text != "" {
			collapsed.WriteString(p.Text)
		}
	}

	msgs = append(msgs, message{
		ID:      uuid4(),
		Author:  msgAuthor{Role: "user"},
		Content: msgContent{ContentType: "text", Parts: []string{collapsed.String()}},
	})

	body := map[string]any{
		"action":                        "next",
		"messages":                      msgs,
		"parent_message_id":             uuid4(),
		"model":                         chatgptModel,
		"timezone_offset_min":           -480,
		"suggestions":                   []any{},
		"history_and_training_disabled": true,
		"conversation_mode":             map[string]string{"kind": "primary_assistant"},
		"force_paragen":                 false,
		"force_paragen_model_slug":      "",
		"force_nulligen":                false,
		"force_rate_limit":              false,
		"reset_rate_limits":             false,
		"websocket_request_id":          uuid4(),
		"system_hints":                  []any{},
		"force_use_sse":                 true,
		"conversation_origin":           nil,
		"client_contextual_info": map[string]any{
			"is_dark_mode":      false,
			"time_since_loaded": 30,
			"page_height":       900,
			"page_width":        1440,
			"pixel_ratio":       2,
			"screen_height":     900,
			"screen_width":      1440,
		},
		"paragen_stream_type_override":         nil,
		"paragen_cot_summary_display_override": "allow",
		"supports_buffering":                   true,
	}
	return json.Marshal(body)
}

// mapToChatgptModelSlug converts API-style model ids (gpt-4o, o1-mini, ...)
// to the slug ChatGPT web expects (gpt-4o, o1-mini-2024-09-12, ...).
func mapToChatgptModelSlug(m string) string {
	switch m {
	case "gpt-4o":
		return "gpt-4o"
	case "gpt-4o-mini":
		return "gpt-4o-mini"
	case "o1-mini":
		return "o1-mini"
	case "o1":
		return "o1"
	case "gpt-4-turbo":
		return "gpt-4"
	}
	// pass through for newer models we don't know about yet
	return m
}

const maxCodexSessionHeaderLen = 512

func setCodexSessionHeaders(h http.Header, responsesBody []byte) {
	sessionID, threadID := codexSessionHeadersFromResponsesBody(responsesBody)
	h.Set("session-id", sessionID)
	h.Set("session_id", sessionID)
	if threadID != "" {
		h.Set("thread-id", threadID)
		h.Set("thread_id", threadID)
		h.Set("x-client-request-id", threadID)
	}
}

func codexSessionHeadersFromResponsesBody(body []byte) (sessionID, threadID string) {
	cacheKey := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	if validCodexSessionHeaderValue(cacheKey) {
		return cacheKey, cacheKey
	}
	return uuid4(), ""
}

func validCodexSessionHeaderValue(v string) bool {
	if v == "" || len(v) > maxCodexSessionHeaderLen {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] == 0x7f {
			return false
		}
	}
	return true
}

func uuid4() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000-0000-0000-0000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}
