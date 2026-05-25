package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/llm-pool/gateway/internal/domain"
	"github.com/llm-pool/gateway/internal/enrollment"
	"github.com/llm-pool/gateway/internal/store"
)

// SetEnrollment wires the singleton enrollment manager (called from main.go).
func (s *Server) SetEnrollment(m *enrollment.Manager) {
	s.enroll = m
}

// ---- Admin: GET /accounts/enroll → form to start enrollment ----

func (s *Server) handleAccountEnrollForm(w http.ResponseWriter, r *http.Request) {
	tenants, _ := s.deps.Store.ListDynTenants(r.Context())
	s.render(w, r, "account_enroll_form.html", map[string]any{
		"Active":      "accounts",
		"Title":       s.t(r, "enroll.title"),
		"YamlTenants": s.deps.Cfg.Tenants,
		"Tenants":     tenants,
	})
}

// ---- Admin: POST /accounts/enroll → create pending enrollment, show bookmarklet ----

func (s *Server) handleAccountEnrollCreate(w http.ResponseWriter, r *http.Request) {
	if s.enroll == nil {
		http.Error(w, "enrollment manager not wired", 500)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	tenantID := r.FormValue("tenant_id")
	if tenantID == "" {
		tenantID = "default"
	}
	provider := r.FormValue("provider")
	if provider == "" {
		provider = "chatgpt"
	}
	note := r.FormValue("note")
	pending := s.enroll.Create(tenantID, provider, "", note)
	http.Redirect(w, r, "/accounts/enroll/"+pending.ID, http.StatusSeeOther)
}

// ---- Admin: GET /accounts/enroll/{id} → bookmarklet + polling page ----

func (s *Server) handleAccountEnrollShow(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if s.enroll == nil {
		http.Error(w, "enrollment manager not wired", 500)
		return
	}
	p, ok := s.enroll.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Build the public URL where the bookmarklet posts back. We trust
	// X-Forwarded-Host / Host (admin is behind the same domain as gateway in
	// the recommended deploy).
	host := publicHost(r)
	postURL := fmt.Sprintf("%s/enroll/%s", host, id)
	bookmarklet := buildBookmarklet(p.Provider, postURL)
	s.render(w, r, "account_enroll_show.html", map[string]any{
		"Active":      "accounts",
		"Title":       s.t(r, "enroll.title"),
		"Pending":     p,
		"PostURL":     postURL,
		"Bookmarklet": bookmarklet,
	})
}

// ---- Admin: GET /accounts/enroll/{id}/status (polled by JS) ----

func (s *Server) handleAccountEnrollStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if s.enroll == nil {
		errJSON(w, 500, "no enrollment")
		return
	}
	p, ok := s.enroll.Get(id)
	if !ok {
		errJSON(w, 404, "not found")
		return
	}
	writeJSONStatus(w, 200, map[string]any{
		"id":           p.ID,
		"state":        string(p.State),
		"account_id":   p.AccountID,
		"error":        p.Error,
		"expires_at":   p.ExpiresAt,
		"completed_at": p.CompletedAt,
	})
}

// ---- Public: POST /enroll/{id} (called by bookmarklet from chatgpt.com origin) ----
//
// CORS-open. Body shape:
//   {"session": "<JSON from /api/auth/session OR raw JSON>",
//    "cookies": "name=val; name=val",
//    "ua": "Mozilla/5.0 ..."}

func (s *Server) handleEnrollSubmit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(204)
		return
	}
	id := chi.URLParam(r, "id")
	if s.enroll == nil {
		http.Error(w, "enrollment manager not wired", 500)
		return
	}
	p, ok := s.enroll.Get(id)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	if p.State != enrollment.StatePending {
		http.Error(w, "enrollment "+string(p.State), 410)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.enroll.Fail(id, err.Error())
		http.Error(w, err.Error(), 400)
		return
	}
	if len(body) > 64*1024 {
		s.enroll.Fail(id, "payload too large")
		http.Error(w, "payload too large", 413)
		return
	}
	var payload struct {
		Session string `json:"session"`
		Cookies string `json:"cookies"`
		UA      string `json:"ua"`
		Origin  string `json:"origin"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		s.enroll.Fail(id, "bad json: "+err.Error())
		http.Error(w, "bad json", 400)
		return
	}
	accID, err := s.completeWebSessionEnrollment(r.Context(), p, strings.TrimSpace(payload.Session), payload.Cookies, payload.UA, false)
	if err != nil {
		s.enroll.Fail(id, err.Error())
		http.Error(w, err.Error(), 400)
		return
	}
	s.enroll.Complete(id, accID)
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "enrollment", accID, "", "account enrolled via bookmarklet (provider="+p.Provider+")")
	}
	writeJSONStatus(w, 200, map[string]any{"ok": true, "account_id": accID})
}

// ---- helpers ----

func (s *Server) completeWebSessionEnrollment(ctx context.Context, p *enrollment.Pending, sess, cookies, ua string, requireCookies bool) (string, error) {
	if p == nil {
		return "", fmt.Errorf("missing enrollment")
	}
	// Validate the session blob looks plausible. For ChatGPT we expect either
	// a JSON object with accessToken or the raw JWE cookie that starts with eyJ.
	sess = strings.TrimSpace(sess)
	if sess == "" {
		return "", fmt.Errorf("empty session")
	}
	if p.Provider == "chatgpt" {
		if !strings.HasPrefix(sess, "{") && !strings.HasPrefix(sess, "eyJ") {
			return "", fmt.Errorf("session does not look like ChatGPT")
		}
		if requireCookies && !looksLikeChatGPTSessionCookies(cookies) {
			return "", fmt.Errorf("cookies must include __Secure-next-auth.session-token for automatic web session refresh")
		}
	}
	accID := "acc-" + randHex(6)
	a := &domain.Account{
		ID:             accID,
		TenantID:       p.TenantID,
		Provider:       p.Provider,
		Email:          accountEmailFromImport("", sess),
		StealthProfile: "chrome_124_windows",
		UA:             ua,
		State:          domain.StateActive,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	sec := store.AccountSecret{
		SessionToken: sess,
		Cookies:      []byte(cookies),
	}
	if err := s.deps.Store.UpsertAccount(ctx, a, sec); err != nil {
		return "", err
	}
	if s.deps.Sched != nil {
		s.deps.Sched.Register(a)
	}
	return accID, nil
}

func looksLikeChatGPTSessionCookies(cookies string) bool {
	low := strings.ToLower(strings.TrimSpace(cookies))
	return strings.Contains(low, "__secure-next-auth.session-token") ||
		strings.Contains(low, "next-auth.session-token")
}

func publicHost(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}

// buildBookmarklet returns the URL-encoded `javascript:` payload that the
// admin drag-and-drops to their bookmarks bar.
func buildBookmarklet(provider, postURL string) string {
	var grabSess string
	switch provider {
	case "chatgpt":
		grabSess = `await fetch('/api/auth/session',{credentials:'include'}).then(r=>r.text())`
	case "claude":
		grabSess = `await fetch('/api/auth/current_account',{credentials:'include'}).then(r=>r.text())`
	default:
		grabSess = `document.cookie`
	}
	js := `(async()=>{try{` +
		`const s=` + grabSess + `;` +
		`const r=await fetch('` + postURL + `',{method:'POST',headers:{'Content-Type':'application/json'},` +
		`body:JSON.stringify({session:s,cookies:document.cookie,ua:navigator.userAgent,origin:location.origin})});` +
		`if(r.ok){alert('LLM Pool: 录入成功 ✓ 可关闭此页');}` +
		`else{alert('LLM Pool: 录入失败 '+(await r.text()));}` +
		`}catch(e){alert('LLM Pool: 错误 '+e.message);}})();`
	return "javascript:" + url.PathEscape(js)
}
