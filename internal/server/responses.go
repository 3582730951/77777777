package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"

	"github.com/llm-pool/gateway/internal/auth"
	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/metrics"
	"github.com/llm-pool/gateway/internal/protocol/openai"
	"github.com/llm-pool/gateway/internal/scheduler"
	"github.com/llm-pool/gateway/internal/stealth"
	"github.com/llm-pool/gateway/internal/store"
	"github.com/llm-pool/gateway/internal/stream"
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// RawInvoker is implemented by providers that support raw Responses API passthrough.
type RawInvoker interface {
	InvokeRaw(ctx interface{}, accountID string, body []byte) (io.ReadCloser, int, error)
}

// handleResponses handles /v1/responses for Codex CLI.
// If the provider supports RawInvoker (chatgpt does), it proxies the raw SSE verbatim.
// Otherwise falls back to IR decode → serveRequest → OpenAI encode.
func (g *Gateway) handleResponses(w http.ResponseWriter, r *http.Request) {
	if isWebSocketUpgrade(r) {
		g.handleResponsesWS(w, r)
		return
	}

	res, _ := r.Context().Value(ctxResolved).(auth.Resolved)
	body, releaseBody, err := g.readRequestBody(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errResp("read_body", err.Error()))
		return
	}
	defer releaseBody()
	body = stealth.ScrubRequestBody(body)

	if res.Group == nil {
		writeJSON(w, http.StatusInternalServerError, errResp("no_group", "resolved group missing"))
		return
	}
	body = g.optimizeResponsesToolOutputs(body, res.Group.Provider)

	// Try raw passthrough first (chatgpt provider).
	prov, ok := g.providers.Get(res.Group.Provider)
	if ok {
		if raw, isRaw := prov.(RawInvoker); isRaw {
			g.handleResponsesPassthrough(w, r, res, body, raw)
			return
		}
	}

	// Fallback: decode as Responses/Chat Completions → IR → serve normally.
	irReq, err := openai.DecodeBytes(body)
	if err != nil {
		metrics.TranslationErrors.WithLabelValues("responses", "ir").Inc()
		writeJSON(w, http.StatusBadRequest, errResp("decode_error", err.Error()))
		return
	}
	g.serveRequest(w, r, res, irReq, "openai")
}

// handleResponsesPassthrough proxies the raw request to upstream and streams SSE back.
func (g *Gateway) handleResponsesPassthrough(w http.ResponseWriter, r *http.Request, res auth.Resolved, body []byte, raw RawInvoker) {
	start := time.Now()
	model := gjson.GetBytes(body, "model").String()
	prevID := gjson.GetBytes(body, "previous_response_id").String()
	prev, hasPrev := g.responses.Lookup(prevID)
	threadKey := responsesThreadKey(body, res.Group)
	threadAffinity, hasThreadAffinity := g.responses.LookupThread(threadKey)
	threadReplay, hasThreadReplay := g.responses.LookupThreadTranscript(threadKey)
	promptKnownInjected := (hasPrev && prev.SystemPromptInjected) ||
		(hasThreadAffinity && threadAffinity.SystemPromptInjected)
	promptRequired := (hasPrev && prev.RequireSystemPrompt) ||
		(hasThreadAffinity && threadAffinity.RequireSystemPrompt)
	currentInput := responsesInputItems(body)
	replayTranscript := prev.Transcript
	if len(replayTranscript) == 0 && hasThreadReplay {
		replayTranscript = threadReplay.Transcript
	}
	hasReplay := len(replayTranscript) > 0
	logicalTranscript := currentInput
	expandedBody := body
	if hasReplay {
		logicalTranscript = mergeResponsesTranscript(replayTranscript, currentInput)
		expandedBody = expandResponsesBody(body, logicalTranscript)
	}

	pickReq := scheduler.PickRequest{
		GroupID:    res.Group.ID,
		TenantID:   res.Group.TenantID,
		Provider:   res.Group.Provider,
		Model:      model,
		AccountIDs: res.Group.AccountIDs,
	}
	contextAccountID := ""
	if hasPrev {
		contextAccountID = prev.AccountID
	} else if prevID != "" && hasThreadReplay {
		contextAccountID = threadReplay.AccountID
	} else if prevID != "" && hasThreadAffinity {
		contextAccountID = threadAffinity.AccountID
	}
	if contextAccountID != "" {
		if !hasReplay || !g.sched.AccountDraining(contextAccountID) {
			pickReq.PreferredAccountID = contextAccountID
		}
		if !hasReplay {
			pickReq.AccountIDs = []string{contextAccountID}
		}
	} else if hasThreadAffinity && !g.sched.AccountDraining(threadAffinity.AccountID) {
		pickReq.PreferredAccountID = threadAffinity.AccountID
	}

	maxAttempts := g.cfg.Scheduler.Retry.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	headCfg := stream.HeadBufferConfig{
		MaxDuration: g.cfg.Scheduler.Failover.HeadBuffer.MaxDuration,
		MaxBytes:    g.cfg.Scheduler.Failover.HeadBuffer.MaxBytes,
		MaxEvents:   g.cfg.Scheduler.Failover.HeadBuffer.MaxEvents,
	}

	var (
		lastErr        error
		lastStatus     int
		lastBody       []byte
		lastAuthErr    error
		lastAuthStatus int
		lastAuthBody   []byte
		lastAccountID  string
		excluded       []string
		committed      bool
	)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		pickReq.ExcludeAccountIDs = excluded
		slot, err := g.sched.Pick(r.Context(), pickReq)
		if err != nil {
			lastErr = err
			break
		}
		lastAccountID = slot.Account.ID

		attemptBody := body
		attemptTranscript := currentInput
		if contextAccountID != "" {
			attemptTranscript = logicalTranscript
			if slot.Account.ID != contextAccountID {
				if !hasReplay {
					lastErr = fmt.Errorf("previous_response_id %s is pinned to account %s and replay transcript is not available", prevID, contextAccountID)
					break
				}
				attemptBody = expandedBody
			}
		}
		promptApplied := shouldApplyResponsesSystemPrompt(res.Group, contextAccountID, slot.Account.ID, promptKnownInjected, promptRequired)
		if promptApplied {
			attemptBody = applyGroupSystemPromptToResponsesBody(attemptBody, res.Group)
		}
		attemptBody = enhanceCyberContextRawBody(attemptBody, res.Group.ID)
		if g.identity != nil {
			rewriter, rerr := g.identity.RewriterForAccount(slot.Account.ID, slot.Account.Provider, slot.Account.Email)
			if rerr != nil {
				lastErr = fmt.Errorf("identity rewrite: %w", rerr)
				break
			}
			attemptBody = rewriter.RewriteJSONBody(attemptBody)
		}
		promptEffective := responsesSystemPromptEffective(res.Group, promptApplied, promptKnownInjected)

		g.sched.IncInflight(slot.Account.ID)
		reader, status, err := raw.InvokeRaw(r.Context(), slot.Account.ID, attemptBody)
		if err != nil {
			g.sched.DecInflight(slot.Account.ID)
			class := scheduler.ClassifyError(0, "", err)
			g.sched.MarkFailure(slot.Account.ID, class)
			g.recordPassthrough(res, model, slot.Account.ID, start, "error")
			if class == domain.ErrAuthFailed {
				lastAuthErr = err
				lastAuthStatus = 0
				lastAuthBody = nil
			}
			excluded = append(excluded, slot.Account.ID)
			lastErr = err
			continue
		}

		if status != http.StatusOK {
			respBody, _ := io.ReadAll(reader)
			reader.Close()
			g.sched.DecInflight(slot.Account.ID)
			class := scheduler.ClassifyError(status, string(respBody), nil)
			g.sched.MarkFailure(slot.Account.ID, class)
			g.recordPassthrough(res, model, slot.Account.ID, start, "error")
			if class == domain.ErrAuthFailed {
				lastAuthErr = fmt.Errorf("upstream %d: %s", status, string(respBody[:minInt(len(respBody), 300)]))
				lastAuthStatus = status
				lastAuthBody = append([]byte(nil), respBody...)
			}
			excluded = append(excluded, slot.Account.ID)
			lastStatus = status
			lastBody = respBody
			lastErr = fmt.Errorf("upstream %d: %s", status, string(respBody[:minInt(len(respBody), 300)]))
			continue
		}

		flusher, _ := w.(http.Flusher)
		result := forwardResponsesSSEWithHeadBuffer(r.Context(), reader, w, flusher, headCfg)
		reader.Close()
		g.sched.DecInflight(slot.Account.ID)
		committed = result.Committed

		if result.Retryable && !result.Committed {
			class := result.Class
			if class == "" {
				class = scheduler.ClassifyError(0, "", result.Err)
			}
			g.sched.MarkFailure(slot.Account.ID, class)
			g.recordPassthrough(res, model, slot.Account.ID, start, "error")
			excluded = append(excluded, slot.Account.ID)
			lastErr = result.Err
			continue
		}

		if result.Err != nil {
			class := scheduler.ClassifyError(0, "", result.Err)
			g.sched.MarkFailure(slot.Account.ID, class)
			g.recordPassthrough(res, model, slot.Account.ID, start, "error")
			lastErr = result.Err
			return
		}

		dur := time.Since(start).Seconds()
		g.sched.MarkSuccess(slot.Account.ID, dur*1000)
		g.recordPassthroughWithUsage(res, model, slot.Account.ID, start, "ok", result.Usage)
		storedThreadTranscript := false
		if result.ResponseID != "" {
			if (prevID == "" || hasReplay) && !result.TranscriptTooLarge {
				finalTranscript := appendAssistantOutput(attemptTranscript, result.AssistantText, result.AssistantItems)
				if threadKey != "" {
					g.responses.RecordWithThreadPrompt(result.ResponseID, slot.Account.ID, threadKey, finalTranscript, promptEffective)
					storedThreadTranscript = true
				} else {
					g.responses.RecordWithPrompt(result.ResponseID, slot.Account.ID, finalTranscript, promptEffective)
				}
			} else {
				g.responses.RecordAccountWithPrompt(result.ResponseID, slot.Account.ID, promptEffective)
			}
		}
		if threadKey != "" && !storedThreadTranscript {
			g.responses.RecordThreadPrompt(threadKey, slot.Account.ID, promptEffective)
		}
		if g.QuotaRefreshFunc != nil {
			go g.QuotaRefreshFunc(slot.Account.ID)
		}
		return
	}

	if committed {
		return
	}
	if msg, ok := passthroughAuthFailureMessage(lastStatus, lastBody, lastErr, lastAuthStatus, lastAuthBody, lastAuthErr); ok {
		writeJSON(w, http.StatusUnauthorized, errResp("auth_failed", msg))
		return
	}
	if lastStatus != 0 && len(lastBody) > 0 {
		w.WriteHeader(lastStatus)
		_, _ = w.Write(lastBody)
		return
	}
	if lastErr == nil {
		lastErr = errors.New("all responses attempts exhausted")
	}
	if lastAccountID != "" {
		g.recordPassthrough(res, model, lastAccountID, start, "error")
	}
	writeJSON(w, http.StatusBadGateway, errResp("upstream_error", lastErr.Error()))
}

func passthroughAuthFailureMessage(lastStatus int, lastBody []byte, lastErr error, authStatus int, authBody []byte, authErr error) (string, bool) {
	if scheduler.ClassifyError(lastStatus, string(lastBody), lastErr) == domain.ErrAuthFailed {
		return authFailureMessage(lastBody, lastErr), true
	}
	if authErr != nil || authStatus != 0 || len(authBody) > 0 {
		if scheduler.ClassifyError(authStatus, string(authBody), authErr) == domain.ErrAuthFailed {
			return authFailureMessage(authBody, authErr), true
		}
	}
	return "", false
}

func authFailureMessage(body []byte, err error) string {
	if err != nil {
		return err.Error()
	}
	if len(body) > 0 {
		return string(body[:minInt(len(body), 300)])
	}
	return "upstream authentication failed"
}

// passthroughUsage holds token counts extracted from SSE during passthrough.
type passthroughUsage struct {
	InputTokens  int
	OutputTokens int
	CachedTokens int
}

type responsesForwardResult struct {
	Usage              passthroughUsage
	ResponseID         string
	AssistantText      string
	AssistantItems     []json.RawMessage
	TranscriptTooLarge bool
	Committed          bool
	Retryable          bool
	Class              domain.ErrorClass
	Err                error
}

type rawChunk struct {
	data []byte
	err  error
}

func forwardResponsesSSEWithHeadBuffer(
	ctx context.Context,
	reader io.Reader,
	w http.ResponseWriter,
	flusher http.Flusher,
	cfg stream.HeadBufferConfig,
) (result responsesForwardResult) {
	cfg = saneRawHeadBuffer(cfg)
	ch := make(chan rawChunk, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				select {
				case ch <- rawChunk{data: cp}:
				case <-done:
					return
				}
			}
			if err != nil {
				select {
				case ch <- rawChunk{err: err}:
				case <-done:
				}
				return
			}
		}
	}()

	var (
		inspector  responsesSSEInspector
		head       bytes.Buffer
		committed  bool
		headerSent bool
	)
	defer func() {
		result.Usage = inspector.Usage
		result.ResponseID = inspector.ResponseID
		result.AssistantText = inspector.AssistantText.String()
		result.AssistantItems = cloneRawMessages(inspector.AssistantItems)
		result.TranscriptTooLarge = inspector.CaptureOverflow
	}()
	timer := time.NewTimer(cfg.MaxDuration)
	defer timer.Stop()

	flushHead := func() error {
		if committed {
			return nil
		}
		if !headerSent {
			writeResponsesSSEHeaders(w)
			headerSent = true
		}
		committed = true
		result.Committed = true
		if head.Len() > 0 {
			if _, err := w.Write(head.Bytes()); err != nil {
				return err
			}
			head.Reset()
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			result.Err = ctx.Err()
			return result
		case <-timer.C:
			if err := flushHead(); err != nil {
				result.Err = err
				return result
			}
		case chunk := <-ch:
			if len(chunk.data) > 0 {
				signal := inspector.Feed(chunk.data)
				if signal.Retryable && !committed {
					result.Retryable = true
					result.Class = signal.Class
					result.Err = signal.Err
					return result
				}
				if !committed {
					_, _ = head.Write(chunk.data)
					if head.Len() >= cfg.MaxBytes || inspector.EventCount >= cfg.MaxEvents {
						if err := flushHead(); err != nil {
							result.Err = err
							return result
						}
					}
					continue
				}
				if !headerSent {
					writeResponsesSSEHeaders(w)
					headerSent = true
				}
				if _, err := w.Write(chunk.data); err != nil {
					result.Err = err
					return result
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if chunk.err != nil {
				if errors.Is(chunk.err, io.EOF) {
					if !committed {
						if err := flushHead(); err != nil {
							result.Err = err
						}
					}
					return result
				}
				if !committed {
					result.Retryable = true
					result.Class = domain.ErrNetwork
				}
				result.Err = chunk.err
				return result
			}
		}
	}
}

func saneRawHeadBuffer(cfg stream.HeadBufferConfig) stream.HeadBufferConfig {
	if cfg.MaxDuration <= 0 {
		cfg.MaxDuration = 500 * time.Millisecond
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 16 * 1024
	}
	if cfg.MaxEvents <= 0 {
		cfg.MaxEvents = 5
	}
	return cfg
}

func writeResponsesSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// forwardSSEAndExtractUsage streams SSE from reader to w while parsing
// response.completed events to extract usage/cache data.
func forwardSSEAndExtractUsage(reader io.Reader, w io.Writer, flusher http.Flusher) passthroughUsage {
	var usage passthroughUsage
	var lineBuf []byte
	var currentEvent string
	tmp := make([]byte, 4096)

	for {
		n, readErr := reader.Read(tmp)
		if n > 0 {
			w.Write(tmp[:n])
			if flusher != nil {
				flusher.Flush()
			}
			// Parse SSE lines to find response.completed
			lineBuf = append(lineBuf, tmp[:n]...)
			if len(lineBuf) > 1<<20 && bytes.IndexByte(lineBuf, '\n') < 0 {
				lineBuf = lineBuf[len(lineBuf)-4096:]
			}
			for {
				idx := bytes.IndexByte(lineBuf, '\n')
				if idx < 0 {
					break
				}
				line := string(bytes.TrimRight(lineBuf[:idx], "\r"))
				lineBuf = lineBuf[idx+1:]

				if strings.HasPrefix(line, "event: ") {
					currentEvent = strings.TrimSpace(line[7:])
				} else if strings.HasPrefix(line, "data: ") && (currentEvent == "response.completed" || currentEvent == "response.incomplete") {
					data := line[6:]
					var d struct {
						Response struct {
							Usage struct {
								InputTokens        int `json:"input_tokens"`
								OutputTokens       int `json:"output_tokens"`
								InputTokensDetails struct {
									CachedTokens int `json:"cached_tokens"`
								} `json:"input_tokens_details"`
							} `json:"usage"`
						} `json:"response"`
					}
					if json.Unmarshal([]byte(data), &d) == nil {
						usage.InputTokens = d.Response.Usage.InputTokens
						usage.OutputTokens = d.Response.Usage.OutputTokens
						usage.CachedTokens = d.Response.Usage.InputTokensDetails.CachedTokens
					}
				} else if line == "" {
					currentEvent = ""
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	return usage
}

type responsesSignal struct {
	Retryable bool
	Class     domain.ErrorClass
	Err       error
}

type responsesSSEInspector struct {
	lineBuf            []byte
	currentEvent       string
	switchSignalWindow string
	EventCount         int
	Usage              passthroughUsage
	ResponseID         string
	AssistantText      strings.Builder
	AssistantItems     []json.RawMessage
	CaptureBytes       int
	CaptureOverflow    bool
}

const responsesSwitchSignalWindowLimit = 4096

var responsesTranscriptCaptureMaxBytes = 16 << 20

func (p *responsesSSEInspector) Feed(data []byte) responsesSignal {
	p.lineBuf = append(p.lineBuf, data...)
	var signal responsesSignal
	for {
		idx := bytes.IndexByte(p.lineBuf, '\n')
		if idx < 0 {
			break
		}
		line := string(bytes.TrimRight(p.lineBuf[:idx], "\r"))
		p.lineBuf = p.lineBuf[idx+1:]
		if len(p.lineBuf) > 1<<20 && bytes.IndexByte(p.lineBuf, '\n') < 0 {
			p.lineBuf = p.lineBuf[len(p.lineBuf)-4096:]
		}
		if line == "" {
			p.EventCount++
			p.currentEvent = ""
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			p.currentEvent = strings.TrimSpace(line[7:])
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := line[6:]
		if data == "[DONE]" {
			continue
		}
		if sig := p.inspectData(data); sig.Retryable && !signal.Retryable {
			signal = sig
		}
	}
	return signal
}

func (p *responsesSSEInspector) inspectData(data string) responsesSignal {
	var evt struct {
		Type     string          `json:"type"`
		Delta    string          `json:"delta"`
		Text     string          `json:"text"`
		Item     json.RawMessage `json:"item"`
		Response struct {
			ID    string `json:"id"`
			Usage struct {
				InputTokens        int `json:"input_tokens"`
				OutputTokens       int `json:"output_tokens"`
				InputTokensDetails struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(data), &evt) != nil {
		return responsesSignal{}
	}
	if evt.Response.ID != "" {
		p.ResponseID = evt.Response.ID
	}
	if evt.Response.Usage.InputTokens > 0 || evt.Response.Usage.OutputTokens > 0 || evt.Response.Usage.InputTokensDetails.CachedTokens > 0 {
		p.Usage.InputTokens = evt.Response.Usage.InputTokens
		p.Usage.OutputTokens = evt.Response.Usage.OutputTokens
		p.Usage.CachedTokens = evt.Response.Usage.InputTokensDetails.CachedTokens
	}
	switch evt.Type {
	case "response.output_item.done":
		if len(evt.Item) > 0 && string(evt.Item) != "null" {
			p.captureAssistantItem(evt.Item)
		}
	case "response.output_text.delta":
		if evt.Delta != "" {
			p.captureAssistantText(evt.Delta)
			if len(evt.Delta) >= responsesSwitchSignalWindowLimit {
				p.switchSignalWindow = evt.Delta[len(evt.Delta)-responsesSwitchSignalWindowLimit:]
			} else {
				p.switchSignalWindow += evt.Delta
				if len(p.switchSignalWindow) > responsesSwitchSignalWindowLimit {
					p.switchSignalWindow = p.switchSignalWindow[len(p.switchSignalWindow)-responsesSwitchSignalWindowLimit:]
				}
			}
			if sig := classifyResponsesSwitchSignal(p.switchSignalWindow, ""); sig.Retryable {
				return sig
			}
		}
	case "response.output_text.done":
		if evt.Text != "" {
			if p.AssistantText.Len() == 0 {
				p.captureAssistantText(evt.Text)
			}
			if sig := classifyResponsesSwitchSignal(evt.Text, ""); sig.Retryable {
				return sig
			}
		}
	case "response.error", "error":
		return classifyResponsesSwitchSignal(evt.Error.Message, evt.Error.Type)
	}
	if p.currentEvent == "response.error" || p.currentEvent == "error" {
		return classifyResponsesSwitchSignal(evt.Error.Message, evt.Error.Type)
	}
	return responsesSignal{}
}

func (p *responsesSSEInspector) captureAssistantText(text string) {
	if text == "" || p.CaptureOverflow {
		return
	}
	remaining := responsesTranscriptCaptureMaxBytes - p.CaptureBytes
	if remaining <= 0 {
		p.CaptureOverflow = true
		return
	}
	if len(text) > remaining {
		p.AssistantText.WriteString(text[:remaining])
		p.CaptureBytes += remaining
		p.CaptureOverflow = true
		return
	}
	p.AssistantText.WriteString(text)
	p.CaptureBytes += len(text)
}

func (p *responsesSSEInspector) captureAssistantItem(item json.RawMessage) {
	if len(item) == 0 || p.CaptureOverflow {
		return
	}
	remaining := responsesTranscriptCaptureMaxBytes - p.CaptureBytes
	if remaining < len(item) {
		p.CaptureOverflow = true
		return
	}
	cp := make([]byte, len(item))
	copy(cp, item)
	p.AssistantItems = append(p.AssistantItems, json.RawMessage(cp))
	p.CaptureBytes += len(item)
}

func classifyResponsesSwitchSignal(message, typ string) responsesSignal {
	text := strings.ToLower(strings.TrimSpace(message + " " + typ))
	if text == "" {
		return responsesSignal{}
	}
	switch {
	case strings.Contains(text, "you've hit your usage limit"),
		strings.Contains(text, "you have hit your usage limit"),
		strings.Contains(text, "usage limit has been reached"),
		strings.Contains(text, "insufficient_quota"),
		strings.Contains(text, "quota exceeded"),
		strings.Contains(text, "usage exhausted"):
		return responsesSignal{
			Retryable: true,
			Class:     domain.ErrQuotaExhausted,
			Err:       fmt.Errorf("upstream quota: %s", message),
		}
	case strings.Contains(text, "selected model is at capacity"),
		strings.Contains(text, "model is at capacity"),
		strings.Contains(text, "please try a different model"),
		strings.Contains(text, "rate_limit_error"),
		strings.Contains(text, "rate limit"):
		return responsesSignal{
			Retryable: true,
			Class:     domain.ErrRateLimited,
			Err:       fmt.Errorf("upstream capacity: %s", message),
		}
	}
	return responsesSignal{}
}

func responsesInputItems(body []byte) []json.RawMessage {
	raw := gjson.GetBytes(body, "input")
	if !raw.IsArray() {
		return nil
	}
	items := make([]json.RawMessage, 0)
	raw.ForEach(func(_, item gjson.Result) bool {
		if item.Raw != "" {
			items = append(items, json.RawMessage([]byte(item.Raw)))
		}
		return true
	})
	return items
}

func applyGroupSystemPromptToResponsesBody(body []byte, group *domain.Group) []byte {
	prompt, mode, _ := effectiveGroupPrompt(group)
	if prompt == "" {
		return body
	}
	current := ""
	if existing := gjson.GetBytes(body, "instructions"); existing.Exists() && existing.Type == gjson.String {
		current = existing.String()
	}
	next := combineSystemPrompt(current, prompt, mode)
	if next == current {
		return body
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return body
	}
	if existing := gjson.GetBytes(body, "instructions"); existing.Exists() && existing.Index > 0 {
		return replaceJSONValue(body, existing.Index, len(existing.Raw), encoded)
	}
	return insertTopLevelJSONField(body, "instructions", encoded)
}

func shouldApplyResponsesSystemPrompt(group *domain.Group, contextAccountID, selectedAccountID string, knownInjected, required bool) bool {
	prompt, _, _ := effectiveGroupPrompt(group)
	if prompt == "" {
		return false
	}
	if !isChatGPTResponsesThreadOnce(group) {
		return true
	}
	if required {
		return true
	}
	if contextAccountID != "" && selectedAccountID != "" && selectedAccountID != contextAccountID {
		return true
	}
	return !knownInjected
}

func responsesSystemPromptEffective(group *domain.Group, applied, knownInjected bool) bool {
	prompt, _, _ := effectiveGroupPrompt(group)
	return prompt != "" && (applied || knownInjected)
}

func isChatGPTResponsesThreadOnce(group *domain.Group) bool {
	if group == nil || group.Provider != "chatgpt" {
		return false
	}
	_, _, injection := effectiveGroupPrompt(group)
	return strings.EqualFold(strings.TrimSpace(injection), "thread_once")
}

func (g *Gateway) requireResponsesSystemPromptAfterCompact(body []byte, group *domain.Group) {
	if g == nil || g.responses == nil || !isChatGPTResponsesThreadOnce(group) {
		return
	}
	if threadKey := responsesThreadKey(body, group); threadKey != "" {
		g.responses.RequireSystemPromptForThread(threadKey)
	}
	if prevID := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()); prevID != "" {
		g.responses.RequireSystemPromptForResponse(prevID)
	}
}

func combineSystemPrompt(current, groupPrompt, mode string) string {
	if groupPrompt == "" {
		return current
	}
	switch mode {
	case "replace":
		return groupPrompt
	case "append":
		if current == "" {
			return groupPrompt
		}
		return current + "\n\n" + groupPrompt
	default:
		if current == "" {
			return groupPrompt
		}
		return groupPrompt + "\n\n" + current
	}
}

func replaceJSONValue(body []byte, valueStart, rawLen int, encoded []byte) []byte {
	if valueStart < 0 || rawLen < 0 || valueStart+rawLen > len(body) {
		return body
	}
	out := make([]byte, 0, len(body)-rawLen+len(encoded))
	out = append(out, body[:valueStart]...)
	out = append(out, encoded...)
	out = append(out, body[valueStart+rawLen:]...)
	return out
}

func insertTopLevelJSONField(body []byte, key string, encoded []byte) []byte {
	start := 0
	for start < len(body) && isJSONSpace(body[start]) {
		start++
	}
	if start >= len(body) || body[start] != '{' {
		return body
	}
	end := start + 1
	for end < len(body) && isJSONSpace(body[end]) {
		end++
	}
	keyBytes, err := json.Marshal(key)
	if err != nil {
		return body
	}
	field := make([]byte, 0, len(keyBytes)+1+len(encoded)+1)
	field = append(field, keyBytes...)
	field = append(field, ':')
	field = append(field, encoded...)
	if end < len(body) && body[end] != '}' {
		field = append(field, ',')
	}
	out := make([]byte, 0, len(body)+len(field))
	out = append(out, body[:start+1]...)
	out = append(out, field...)
	out = append(out, body[start+1:]...)
	return out
}

func isJSONSpace(b byte) bool {
	switch b {
	case ' ', '\n', '\r', '\t':
		return true
	default:
		return false
	}
}

const maxResponsesThreadKeyLen = 512

func responsesThreadKey(body []byte, group *domain.Group) string {
	key := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	if !validResponsesThreadKey(key) {
		return ""
	}
	if group == nil {
		return key
	}
	return strings.Join([]string{group.TenantID, group.ID, group.Provider, key}, "\x00")
}

func validResponsesThreadKey(v string) bool {
	if v == "" || len(v) > maxResponsesThreadKeyLen {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] == 0x7f {
			return false
		}
	}
	return true
}

func mergeResponsesTranscript(prev, current []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(prev)+len(current))
	out = append(out, cloneRawMessages(prev)...)
	out = append(out, cloneRawMessages(current)...)
	return out
}

func expandResponsesBody(body []byte, transcript []json.RawMessage) []byte {
	if len(transcript) == 0 {
		return body
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return body
	}
	merged, err := json.Marshal(transcript)
	if err != nil {
		return body
	}
	req["input"] = merged
	delete(req, "previous_response_id")
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}

func appendAssistantOutput(transcript []json.RawMessage, text string, items []json.RawMessage) []json.RawMessage {
	out := cloneRawMessages(transcript)
	text = strings.TrimSpace(text)
	if text == "" && len(items) == 0 {
		return out
	}
	if text != "" && !rawMessagesContainType(items, "message") {
		item := map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []map[string]string{
				{"type": "output_text", "text": text},
			},
		}
		b, err := json.Marshal(item)
		if err == nil {
			out = append(out, json.RawMessage(b))
		}
	}
	out = append(out, cloneRawMessages(items)...)
	return out
}

func rawMessagesContainType(items []json.RawMessage, typ string) bool {
	for _, item := range items {
		if gjson.GetBytes(item, "type").String() == typ {
			return true
		}
	}
	return false
}

func cloneRawMessages(in []json.RawMessage) []json.RawMessage {
	if len(in) == 0 {
		return nil
	}
	out := make([]json.RawMessage, 0, len(in))
	for _, item := range in {
		cp := make([]byte, len(item))
		copy(cp, item)
		out = append(out, json.RawMessage(cp))
	}
	return out
}

// recordPassthrough logs a request sample for raw passthrough error paths.
func (g *Gateway) recordPassthrough(res auth.Resolved, model, accountID string, start time.Time, status string) {
	g.recordPassthroughWithUsage(res, model, accountID, start, status, passthroughUsage{})
}

// recordPassthroughWithUsage logs a request sample with token/cache data.
func (g *Gateway) recordPassthroughWithUsage(res auth.Resolved, model, accountID string, start time.Time, status string, usage passthroughUsage) {
	if g.store == nil || res.Group == nil {
		return
	}
	dur := time.Since(start)
	metrics.RequestTotal.WithLabelValues(res.Group.ID, res.Group.Provider, model, status).Inc()
	metrics.RequestDuration.WithLabelValues(res.Group.ID, res.Group.Provider, model).Observe(dur.Seconds())
	_ = g.store.AppendRequestSample(context.Background(), store.RequestSample{
		At:              time.Now(),
		TenantID:        res.Group.TenantID,
		GroupID:         res.Group.ID,
		Provider:        res.Group.Provider,
		Model:           model,
		AccountID:       accountID,
		APIKey:          res.APIKey,
		CacheHit:        usage.CachedTokens > 0,
		LatencyMs:       dur.Milliseconds(),
		InputTokens:     usage.InputTokens,
		OutputTokens:    usage.OutputTokens,
		CacheReadTokens: usage.CachedTokens,
		Status:          status,
	})
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// handleResponsesWS handles WebSocket connections for Codex CLI.
func (g *Gateway) handleResponsesWS(w http.ResponseWriter, r *http.Request) {
	res, _ := r.Context().Value(ctxResolved).(auth.Resolved)

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(g.maxRequestBytes())

	if res.Group == nil {
		wsError(conn, "server_error", "no group configured")
		return
	}

	prov, ok := g.providers.Get(res.Group.Provider)
	if !ok {
		wsError(conn, "server_error", "unknown provider")
		return
	}
	raw, isRaw := prov.(RawInvoker)

	for {
		msg, releaseMsg, err := g.readResponsesWSMessage(r.Context(), conn)
		if err != nil {
			return
		}
		if releaseMsg == nil {
			releaseMsg = func() {}
		}
		if len(msg) == 0 {
			releaseMsg()
			continue
		}

		body := extractResponsesBody(msg)
		if body == nil {
			wsError(conn, "invalid_request_error", "expected response.create")
			releaseMsg()
			continue
		}
		body = stealth.ScrubRequestBody(body)
		body = g.optimizeResponsesToolOutputs(body, res.Group.Provider)

		if !isRaw {
			// Fallback: decode and serve through IR.
			irReq, err := openai.DecodeBytes(body)
			if err != nil {
				wsError(conn, "invalid_request_error", err.Error())
				releaseMsg()
				continue
			}
			irReq.Stream = true
			adapter := &wsResponseAdapter{conn: conn, headers: make(http.Header), ctx: r.Context(), limiter: g.currentEgressLimiter()}
			g.serveRequest(adapter, r, res, irReq, "openai")
			releaseMsg()
			continue
		}

		// Raw passthrough via WebSocket.
		wsStart := time.Now()
		model := gjson.GetBytes(body, "model").String()
		prevID := gjson.GetBytes(body, "previous_response_id").String()
		prev, hasPrev := g.responses.Lookup(prevID)
		threadKey := responsesThreadKey(body, res.Group)
		threadAffinity, hasThreadAffinity := g.responses.LookupThread(threadKey)
		threadReplay, hasThreadReplay := g.responses.LookupThreadTranscript(threadKey)
		promptKnownInjected := (hasPrev && prev.SystemPromptInjected) ||
			(hasThreadAffinity && threadAffinity.SystemPromptInjected)
		promptRequired := (hasPrev && prev.RequireSystemPrompt) ||
			(hasThreadAffinity && threadAffinity.RequireSystemPrompt)
		currentInput := responsesInputItems(body)
		replayTranscript := prev.Transcript
		if len(replayTranscript) == 0 && hasThreadReplay {
			replayTranscript = threadReplay.Transcript
		}
		hasReplay := len(replayTranscript) > 0
		logicalTranscript := currentInput
		expandedBody := body
		if hasReplay {
			logicalTranscript = mergeResponsesTranscript(replayTranscript, currentInput)
			expandedBody = expandResponsesBody(body, logicalTranscript)
		}

		pickReq := scheduler.PickRequest{
			GroupID: res.Group.ID, TenantID: res.Group.TenantID,
			Provider: res.Group.Provider, Model: model,
			AccountIDs: res.Group.AccountIDs,
		}
		contextAccountID := ""
		if hasPrev {
			contextAccountID = prev.AccountID
		} else if prevID != "" && hasThreadReplay {
			contextAccountID = threadReplay.AccountID
		} else if prevID != "" && hasThreadAffinity {
			contextAccountID = threadAffinity.AccountID
		}
		if contextAccountID != "" {
			if !hasReplay || !g.sched.AccountDraining(contextAccountID) {
				pickReq.PreferredAccountID = contextAccountID
			}
			if !hasReplay {
				pickReq.AccountIDs = []string{contextAccountID}
			}
		} else if hasThreadAffinity && !g.sched.AccountDraining(threadAffinity.AccountID) {
			pickReq.PreferredAccountID = threadAffinity.AccountID
		}

		maxAttempts := g.cfg.Scheduler.Retry.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = 3
		}
		headCfg := stream.HeadBufferConfig{
			MaxDuration: g.cfg.Scheduler.Failover.HeadBuffer.MaxDuration,
			MaxBytes:    g.cfg.Scheduler.Failover.HeadBuffer.MaxBytes,
			MaxEvents:   g.cfg.Scheduler.Failover.HeadBuffer.MaxEvents,
		}

		var (
			lastErr        error
			lastStatus     int
			lastBody       []byte
			lastAuthErr    error
			lastAuthStatus int
			lastAuthBody   []byte
			lastAccountID  string
			excluded       []string
			committed      bool
		)
		for attempt := 0; attempt < maxAttempts; attempt++ {
			pickReq.ExcludeAccountIDs = excluded
			slot, err := g.sched.Pick(r.Context(), pickReq)
			if err != nil {
				lastErr = err
				break
			}
			lastAccountID = slot.Account.ID

			attemptBody := body
			attemptTranscript := currentInput
			if contextAccountID != "" {
				attemptTranscript = logicalTranscript
				if slot.Account.ID != contextAccountID {
					if !hasReplay {
						lastErr = fmt.Errorf("previous_response_id %s is pinned to account %s and replay transcript is not available", prevID, contextAccountID)
						break
					}
					attemptBody = expandedBody
				}
			}
			promptApplied := shouldApplyResponsesSystemPrompt(res.Group, contextAccountID, slot.Account.ID, promptKnownInjected, promptRequired)
			if promptApplied {
				attemptBody = applyGroupSystemPromptToResponsesBody(attemptBody, res.Group)
			}
			attemptBody = enhanceCyberContextRawBody(attemptBody, res.Group.ID)
			promptEffective := responsesSystemPromptEffective(res.Group, promptApplied, promptKnownInjected)

			g.sched.IncInflight(slot.Account.ID)
			reader, status, err := raw.InvokeRaw(r.Context(), slot.Account.ID, attemptBody)
			if err != nil {
				g.sched.DecInflight(slot.Account.ID)
				class := scheduler.ClassifyError(0, "", err)
				g.sched.MarkFailure(slot.Account.ID, class)
				g.recordPassthrough(res, model, slot.Account.ID, wsStart, "error")
				if class == domain.ErrAuthFailed {
					lastAuthErr = err
					lastAuthStatus = 0
					lastAuthBody = nil
				}
				excluded = append(excluded, slot.Account.ID)
				lastErr = err
				continue
			}
			if status != http.StatusOK {
				respBody, _ := io.ReadAll(reader)
				reader.Close()
				g.sched.DecInflight(slot.Account.ID)
				class := scheduler.ClassifyError(status, string(respBody), nil)
				g.sched.MarkFailure(slot.Account.ID, class)
				g.recordPassthrough(res, model, slot.Account.ID, wsStart, "error")
				if class == domain.ErrAuthFailed {
					lastAuthErr = fmt.Errorf("upstream %d: %s", status, string(respBody[:minInt(len(respBody), 300)]))
					lastAuthStatus = status
					lastAuthBody = append([]byte(nil), respBody...)
				}
				excluded = append(excluded, slot.Account.ID)
				lastStatus = status
				lastBody = respBody
				lastErr = fmt.Errorf("upstream %d: %s", status, string(respBody[:minInt(len(respBody), 300)]))
				continue
			}

			result := g.forwardResponsesSSEToWSWithHeadBuffer(r.Context(), reader, conn, headCfg)
			reader.Close()
			g.sched.DecInflight(slot.Account.ID)
			committed = result.Committed

			if result.Retryable && !result.Committed {
				class := result.Class
				if class == "" {
					class = scheduler.ClassifyError(0, "", result.Err)
				}
				g.sched.MarkFailure(slot.Account.ID, class)
				g.recordPassthrough(res, model, slot.Account.ID, wsStart, "error")
				excluded = append(excluded, slot.Account.ID)
				lastErr = result.Err
				continue
			}

			if result.Err != nil {
				class := scheduler.ClassifyError(0, "", result.Err)
				g.sched.MarkFailure(slot.Account.ID, class)
				g.recordPassthrough(res, model, slot.Account.ID, wsStart, "error")
				lastErr = result.Err
				break
			}

			wsDur := time.Since(wsStart).Seconds()
			g.sched.MarkSuccess(slot.Account.ID, wsDur*1000)
			g.recordPassthroughWithUsage(res, model, slot.Account.ID, wsStart, "ok", result.Usage)
			storedThreadTranscript := false
			if result.ResponseID != "" {
				if (prevID == "" || hasReplay) && !result.TranscriptTooLarge {
					finalTranscript := appendAssistantOutput(attemptTranscript, result.AssistantText, result.AssistantItems)
					if threadKey != "" {
						g.responses.RecordWithThreadPrompt(result.ResponseID, slot.Account.ID, threadKey, finalTranscript, promptEffective)
						storedThreadTranscript = true
					} else {
						g.responses.RecordWithPrompt(result.ResponseID, slot.Account.ID, finalTranscript, promptEffective)
					}
				} else {
					g.responses.RecordAccountWithPrompt(result.ResponseID, slot.Account.ID, promptEffective)
				}
			}
			if threadKey != "" && !storedThreadTranscript {
				g.responses.RecordThreadPrompt(threadKey, slot.Account.ID, promptEffective)
			}
			if g.QuotaRefreshFunc != nil {
				go g.QuotaRefreshFunc(slot.Account.ID)
			}
			lastErr = nil
			break
		}
		if committed || lastErr == nil {
			releaseMsg()
			continue
		}
		if lastAccountID != "" {
			g.recordPassthrough(res, model, lastAccountID, wsStart, "error")
		}
		if msg, ok := passthroughAuthFailureMessage(lastStatus, lastBody, lastErr, lastAuthStatus, lastAuthBody, lastAuthErr); ok {
			wsError(conn, "auth_failed", msg)
		} else {
			wsError(conn, "upstream_error", lastErr.Error())
		}
		releaseMsg()
	}
}

func (g *Gateway) readResponsesWSMessage(ctx context.Context, conn *websocket.Conn) ([]byte, func(), error) {
	messageType, reader, err := conn.NextReader()
	if err != nil {
		return nil, nil, err
	}
	if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
		return nil, nil, nil
	}
	return g.readAllGated(ctx, &rateLimitedReader{Reader: reader, ctx: ctx, limiter: g.currentIngressLimiter()}, g.maxRequestBytes())
}

func (g *Gateway) forwardResponsesSSEToWSWithHeadBuffer(
	ctx context.Context,
	reader io.Reader,
	conn *websocket.Conn,
	cfg stream.HeadBufferConfig,
) (result responsesForwardResult) {
	cfg = saneRawHeadBuffer(cfg)
	ch := make(chan rawChunk, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				select {
				case ch <- rawChunk{data: cp}:
				case <-done:
					return
				}
			}
			if err != nil {
				select {
				case ch <- rawChunk{err: err}:
				case <-done:
				}
				return
			}
		}
	}()

	var (
		inspector responsesSSEInspector
		head      bytes.Buffer
		committed bool
		writer    = responsesWSForwarder{conn: conn, ctx: ctx, limiter: g.currentEgressLimiter()}
	)
	defer func() {
		result.Usage = inspector.Usage
		result.ResponseID = inspector.ResponseID
		result.AssistantText = inspector.AssistantText.String()
		result.AssistantItems = cloneRawMessages(inspector.AssistantItems)
		result.TranscriptTooLarge = inspector.CaptureOverflow
	}()
	timer := time.NewTimer(cfg.MaxDuration)
	defer timer.Stop()

	flushHead := func() error {
		if committed {
			return nil
		}
		committed = true
		result.Committed = true
		if head.Len() > 0 {
			if err := writer.Write(head.Bytes()); err != nil {
				return err
			}
			head.Reset()
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			result.Err = ctx.Err()
			return result
		case <-timer.C:
			if err := flushHead(); err != nil {
				result.Err = err
				return result
			}
		case chunk := <-ch:
			if len(chunk.data) > 0 {
				signal := inspector.Feed(chunk.data)
				if signal.Retryable && !committed {
					result.Retryable = true
					result.Class = signal.Class
					result.Err = signal.Err
					return result
				}
				if !committed {
					_, _ = head.Write(chunk.data)
					if head.Len() >= cfg.MaxBytes || inspector.EventCount >= cfg.MaxEvents {
						if err := flushHead(); err != nil {
							result.Err = err
							return result
						}
					}
					continue
				}
				if err := writer.Write(chunk.data); err != nil {
					result.Err = err
					return result
				}
			}
			if chunk.err != nil {
				if errors.Is(chunk.err, io.EOF) {
					if !committed {
						if err := flushHead(); err != nil {
							result.Err = err
						}
					}
					return result
				}
				if !committed {
					result.Retryable = true
					result.Class = domain.ErrNetwork
				}
				result.Err = chunk.err
				return result
			}
		}
	}
}

type responsesWSForwarder struct {
	conn    *websocket.Conn
	ctx     context.Context
	limiter *byteRateLimiter
	buf     []byte
}

func (w *responsesWSForwarder) Write(data []byte) error {
	w.buf = append(w.buf, data...)
	for {
		idx, sepLen := nextSSEBlock(w.buf)
		if idx < 0 {
			return nil
		}
		block := w.buf[:idx]
		w.buf = w.buf[idx+sepLen:]
		payload := sseDataPayload(block)
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if err := w.limiter.wait(w.ctx, len(payload)); err != nil {
			return err
		}
		if err := w.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			return err
		}
	}
}

func nextSSEBlock(buf []byte) (int, int) {
	if idx := bytes.Index(buf, []byte("\n\n")); idx >= 0 {
		return idx, 2
	}
	if idx := bytes.Index(buf, []byte("\r\n\r\n")); idx >= 0 {
		return idx, 4
	}
	return -1, 0
}

func sseDataPayload(block []byte) []byte {
	var lines [][]byte
	for _, line := range bytes.Split(block, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		switch {
		case bytes.HasPrefix(line, []byte("data: ")):
			lines = append(lines, append([]byte(nil), line[6:]...))
		case bytes.Equal(line, []byte("data:")):
			lines = append(lines, []byte{})
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return bytes.Join(lines, []byte("\n"))
}

func extractResponsesBody(msg []byte) []byte {
	t := gjson.GetBytes(msg, "type").String()
	if t == "response.create" {
		if resp := gjson.GetBytes(msg, "response"); resp.Exists() {
			return []byte(resp.Raw)
		}
		return msg
	}
	return nil
}

type wsResponseAdapter struct {
	conn    *websocket.Conn
	headers http.Header
	ctx     context.Context
	limiter *byteRateLimiter
	buf     []byte
}

func (w *wsResponseAdapter) Header() http.Header { return w.headers }
func (w *wsResponseAdapter) WriteHeader(int)     {}
func (w *wsResponseAdapter) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	for {
		idx := bytes.Index(w.buf, []byte("\n\n"))
		if idx < 0 {
			break
		}
		line := w.buf[:idx]
		w.buf = w.buf[idx+2:]
		if len(line) > 6 && string(line[:6]) == "data: " {
			payload := line[6:]
			if string(payload) != "[DONE]" {
				if err := w.limiter.wait(w.ctx, len(payload)); err != nil {
					return len(b), err
				}
				w.conn.WriteMessage(websocket.TextMessage, payload)
			}
		}
	}
	return len(b), nil
}
func (w *wsResponseAdapter) Flush() {}

func wsError(conn *websocket.Conn, code, message string) {
	b, _ := json.Marshal(map[string]any{"type": "error", "code": code, "message": message})
	conn.WriteMessage(websocket.TextMessage, b)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
