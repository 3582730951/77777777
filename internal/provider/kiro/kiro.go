package kiro

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/protocol/ir"
	"github.com/llm-pool/gateway/internal/store"
	"github.com/llm-pool/gateway/internal/transport"
)

const (
	refreshURL     = "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken"
	oidcRefreshURL = "https://oidc.us-east-1.amazonaws.com/token"
	baseURL        = "https://q.us-east-1.amazonaws.com/generateAssistantResponse"
	kiroVer        = "0.11.63"
)

var modelMapping = map[string]string{
	"claude-haiku-4-5":          "claude-haiku-4.5",
	"claude-opus-4-7":           "claude-opus-4.7",
	"claude-opus-4-6":           "claude-opus-4.6",
	"claude-sonnet-4-6":         "claude-sonnet-4.6",
	"claude-opus-4-5":           "claude-opus-4.5",
	"claude-sonnet-4-5":         "claude-sonnet-4.5",
	"claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
}

type tokenCache struct {
	accessToken string
	profileArn  string
	expiresAt   time.Time
}

type Provider struct {
	Mode       string
	store      *store.Store
	httpClient *http.Client

	mu     sync.Mutex
	tokens map[string]*tokenCache // accountID -> cache
}

func New(mode string) *Provider {
	if mode == "" {
		mode = "mock"
	}
	return &Provider{
		Mode:       mode,
		httpClient: transport.ForProvider("q.us-east-1.amazonaws.com", transport.Options{Timeout: 120 * time.Second}),
		tokens:     make(map[string]*tokenCache),
	}
}

func (p *Provider) SetStore(s *store.Store) { p.store = s }
func (p *Provider) Name() string            { return "kiro" }

func (p *Provider) Invoke(ctx context.Context, acc *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	if p.Mode == "mock" {
		return p.invokeMock(ctx, acc, req)
	}
	return p.invokeReal(ctx, acc, req)
}

func (p *Provider) Probe(ctx context.Context, acc *domain.Account) error {
	if p.Mode == "mock" {
		return nil
	}
	_, _, err := p.ensureAccessToken(ctx, acc)
	return err
}

func (p *Provider) Discover(ctx context.Context, acc *domain.Account) (*domain.QuotaState, error) {
	models := []domain.ModelCapability{
		{ID: "claude-sonnet-4-5", SupportsTools: true, SupportsVision: true},
		{ID: "claude-sonnet-4-6", SupportsTools: true, SupportsVision: true},
		{ID: "claude-haiku-4-5", SupportsTools: true, SupportsVision: true},
		{ID: "claude-opus-4-5", SupportsTools: true, SupportsVision: true},
		{ID: "claude-opus-4-6", SupportsTools: true, SupportsVision: true},
		{ID: "claude-opus-4-7", SupportsTools: true, SupportsVision: true},
	}
	return &domain.QuotaState{
		ShortWindow:      domain.QuotaWindow{Limit: 50, Used: 0, ResetAt: time.Now().Add(5 * time.Hour), Confidence: 0.5},
		LongWindow:       domain.QuotaWindow{Limit: 200, Used: 0, ResetAt: time.Now().Add(7 * 24 * time.Hour), Confidence: 0.5},
		DiscoveredModels: models,
		LastDiscoveryAt:  time.Now(),
	}, nil
}

// ensureAccessToken refreshes the access token if needed
func (p *Provider) ensureAccessToken(ctx context.Context, acc *domain.Account) (string, string, error) {
	p.mu.Lock()
	if tc, ok := p.tokens[acc.ID]; ok && time.Now().Before(tc.expiresAt.Add(-2*time.Minute)) {
		at, pa := tc.accessToken, tc.profileArn
		p.mu.Unlock()
		return at, pa, nil
	}
	p.mu.Unlock()

	sec, err := p.store.GetAccountSecret(ctx, acc.ID)
	if err != nil {
		return "", "", fmt.Errorf("get secret: %w", err)
	}
	refreshToken := sec.RefreshToken
	if refreshToken == "" {
		refreshToken = sec.SessionToken
	}
	if refreshToken == "" {
		return "", "", errors.New("no refresh token for kiro account")
	}

	// Detect auth type: OIDC (aor prefix + has cookies with clientId) vs Social
	var reqBody []byte
	var refreshEndpoint string

	// Try to parse clientId:clientSecret from Cookies field (JSON encoded)
	var oidcCreds struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if len(sec.Cookies) > 0 {
		_ = json.Unmarshal(sec.Cookies, &oidcCreds)
	}

	if oidcCreds.ClientID != "" && oidcCreds.ClientSecret != "" {
		// OIDC refresh (Builder ID with clientId/clientSecret)
		refreshEndpoint = oidcRefreshURL
		reqBody, _ = json.Marshal(map[string]string{
			"clientId":     oidcCreds.ClientID,
			"clientSecret": oidcCreds.ClientSecret,
			"refreshToken": refreshToken,
			"grantType":    "refresh_token",
		})
	} else {
		// Social auth refresh (Kiro Desktop)
		refreshEndpoint = refreshURL
		reqBody, _ = json.Marshal(map[string]string{"refreshToken": refreshToken})
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, refreshEndpoint, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("refresh token: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("refresh token: status %d: %s", resp.StatusCode, string(raw))
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ProfileArn   string `json:"profileArn"`
		ExpiresIn    int    `json:"expiresIn"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", "", fmt.Errorf("parse refresh response: %w", err)
	}
	if result.AccessToken == "" {
		return "", "", errors.New("refresh response missing accessToken")
	}

	// Update stored refresh token if it changed
	if result.RefreshToken != "" && result.RefreshToken != refreshToken {
		sec.RefreshToken = result.RefreshToken
		_ = p.store.UpsertAccount(ctx, acc, sec)
	}

	expiresIn := result.ExpiresIn
	if expiresIn == 0 {
		expiresIn = 3600
	}

	p.mu.Lock()
	p.tokens[acc.ID] = &tokenCache{
		accessToken: result.AccessToken,
		profileArn:  result.ProfileArn,
		expiresAt:   time.Now().Add(time.Duration(expiresIn) * time.Second),
	}
	p.mu.Unlock()

	return result.AccessToken, result.ProfileArn, nil
}

func (p *Provider) invokeReal(ctx context.Context, acc *domain.Account, req *ir.Request) (<-chan ir.Event, error) {
	accessToken, profileArn, err := p.ensureAccessToken(ctx, acc)
	if err != nil {
		return nil, err
	}

	reqBody := buildCodewhispererRequest(req, profileArn)
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	machineID := fmt.Sprintf("%x", sha256.Sum256([]byte(acc.ID)))[:32]
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+accessToken)
	httpReq.Header.Set("amz-sdk-invocation-id", uuid.NewString())
	httpReq.Header.Set("amz-sdk-request", "attempt=1; max=3")
	httpReq.Header.Set("x-amzn-codewhisperer-optout", "true")
	httpReq.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	httpReq.Header.Set("x-amz-user-agent", fmt.Sprintf("aws-sdk-js/1.0.34 KiroIDE-%s-%s", kiroVer, machineID))
	httpReq.Header.Set("User-Agent", fmt.Sprintf("aws-sdk-js/1.0.34 ua/2.1 os/linux lang/js md/nodejs#20.0.0 api/codewhispererstreaming#1.0.34 m/E KiroIDE-%s-%s", kiroVer, machineID))

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("kiro invoke: %w", err)
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("kiro invoke: status %d: %s", resp.StatusCode, string(b))
	}

	out := make(chan ir.Event, 32)
	go func() {
		defer close(out)
		parseAWSEventStream(ctx, resp.Body, out)
	}()
	return out, nil
}

func buildCodewhispererRequest(req *ir.Request, profileArn string) map[string]any {
	conversationID := uuid.NewString()

	model := req.Model
	if mapped, ok := modelMapping[model]; ok {
		model = mapped
	}

	// Build messages
	var history []map[string]any

	// Collect system prompt
	systemPrompt := req.System

	// Build message list
	var messages []struct {
		role    string
		content string
	}
	for _, m := range req.Messages {
		var content string
		for _, part := range m.Parts {
			if part.Kind == ir.PartText {
				content += part.Text
			}
		}
		role := "user"
		switch m.Role {
		case ir.RoleAssistant:
			role = "assistant"
		case ir.RoleSystem:
			if systemPrompt == "" {
				systemPrompt = content
			} else {
				systemPrompt += "\n" + content
			}
			continue
		}
		messages = append(messages, struct {
			role    string
			content string
		}{role, content})
	}

	if len(messages) == 0 {
		messages = append(messages, struct {
			role    string
			content string
		}{"user", "Hello"})
	}

	// First user message includes system prompt
	startIndex := 0
	firstContent := messages[0].content
	if systemPrompt != "" && messages[0].role == "user" {
		firstContent = systemPrompt + "\n\n" + firstContent
	}
	history = append(history, map[string]any{
		"userInputMessage": map[string]any{
			"content": firstContent,
			"modelId": model,
			"origin":  "AI_EDITOR",
		},
	})
	startIndex = 1

	// Add history (all but last message)
	for i := startIndex; i < len(messages)-1; i++ {
		m := messages[i]
		if m.role == "user" {
			history = append(history, map[string]any{
				"userInputMessage": map[string]any{
					"content": m.content,
					"modelId": model,
					"origin":  "AI_EDITOR",
				},
			})
		} else {
			history = append(history, map[string]any{
				"assistantResponseMessage": map[string]any{
					"content": m.content,
				},
			})
		}
	}

	// Current message (last one)
	var currentContent string
	if len(messages) > 1 {
		last := messages[len(messages)-1]
		if last.role == "assistant" {
			history = append(history, map[string]any{
				"assistantResponseMessage": map[string]any{
					"content": last.content,
				},
			})
			currentContent = "Continue"
		} else {
			// Ensure history ends with assistant message if needed
			if len(history) > 0 {
				lastHist := history[len(history)-1]
				if _, ok := lastHist["assistantResponseMessage"]; !ok {
					history = append(history, map[string]any{
						"assistantResponseMessage": map[string]any{
							"content": "Continue",
						},
					})
				}
			}
			currentContent = last.content
		}
	} else {
		// Only one message, already in history as first user msg
		// Need to set current as a follow-up
		currentContent = firstContent
		history = nil // no history, just current message
	}

	currentMessage := map[string]any{
		"userInputMessage": map[string]any{
			"content": currentContent,
			"modelId": model,
			"origin":  "AI_EDITOR",
			"userInputMessageContext": map[string]any{
				"tools": []map[string]any{
					{
						"toolSpecification": map[string]any{
							"name":        "no_tool_available",
							"description": "Placeholder tool.",
							"inputSchema": map[string]any{
								"json": map[string]any{
									"type":       "object",
									"properties": map[string]any{},
								},
							},
						},
					},
				},
			},
		},
	}

	result := map[string]any{
		"conversationState": map[string]any{
			"agentTaskType":   "vibe",
			"chatTriggerType": "MANUAL",
			"conversationId":  conversationID,
			"currentMessage":  currentMessage,
		},
	}
	if len(history) > 0 {
		result["conversationState"].(map[string]any)["history"] = history
	}
	if profileArn != "" {
		result["profileArn"] = profileArn
	}

	return result
}

// parseAWSEventStream parses the AWS Event Stream binary format
// The stream contains binary-framed messages with JSON payloads like {"content":"text"}
func parseAWSEventStream(ctx context.Context, body io.ReadCloser, out chan<- ir.Event) {
	defer body.Close()

	reader := bufio.NewReaderSize(body, 64*1024)
	var buf bytes.Buffer

	for {
		select {
		case <-ctx.Done():
			out <- ir.Event{Kind: ir.EvError, Err: ctx.Err()}
			return
		default:
		}

		// Read available data
		tmp := make([]byte, 4096)
		n, err := reader.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}

		// Extract JSON payloads from the buffer
		data := buf.Bytes()
		lastConsumed := 0

		for i := 0; i < len(data); i++ {
			if data[i] != '{' {
				continue
			}
			// Try to find matching closing brace
			end := findJSONEnd(data, i)
			if end < 0 {
				break // incomplete, wait for more data
			}
			jsonBytes := data[i : end+1]
			i = end // skip past this JSON

			var ev struct {
				Content                string  `json:"content"`
				ContextUsagePercentage float64 `json:"contextUsagePercentage"`
				FollowupPrompt         any     `json:"followupPrompt"`
			}
			if json.Unmarshal(jsonBytes, &ev) != nil {
				continue
			}
			if ev.Content != "" && ev.FollowupPrompt == nil {
				out <- ir.Event{Kind: ir.EvTextDelta, Text: ev.Content}
			}
			lastConsumed = end + 1
		}

		if lastConsumed > 0 {
			remaining := make([]byte, buf.Len()-lastConsumed)
			copy(remaining, buf.Bytes()[lastConsumed:])
			buf.Reset()
			buf.Write(remaining)
		}

		if err != nil {
			if err != io.EOF {
				out <- ir.Event{Kind: ir.EvError, Err: err}
			}
			break
		}
	}

	out <- ir.Event{Kind: ir.EvDone, FinishReason: "stop"}
}

func findJSONEnd(data []byte, start int) int {
	braceCount := 0
	inString := false
	escape := false
	for i := start; i < len(data); i++ {
		if escape {
			escape = false
			continue
		}
		ch := data[i]
		if ch == '\\' && inString {
			escape = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if !inString {
			if ch == '{' {
				braceCount++
			} else if ch == '}' {
				braceCount--
				if braceCount == 0 {
					return i
				}
			}
		}
	}
	return -1
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
			"[mock kiro provider · account=%s · model=%s]\n\nYou said: %q\n\nMock response from kiro provider.",
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

var _ = binary.BigEndian // suppress unused import
var _ = errors.New
