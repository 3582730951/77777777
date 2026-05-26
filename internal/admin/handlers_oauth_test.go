package admin

import (
	"bytes"
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/enrollment"
	"github.com/llm-pool/gateway/internal/scheduler"
	"github.com/llm-pool/gateway/internal/store"
)

func TestOAuthShowAuthURLKeepsInnerOAuthEncoding(t *testing.T) {
	authURL := "https://auth.openai.com/oauth/authorize?" +
		"response_type=code" +
		"&client_id=app_EMoamEEZ73f0CkXaXp7hrann" +
		"&redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback" +
		"&scope=openid%20profile%20email%20offline_access" +
		"&code_challenge=challenge" +
		"&code_challenge_method=S256" +
		"&id_token_add_organizations=true" +
		"&codex_cli_simplified_flow=true" +
		"&state=state" +
		"&originator=codex_cli_rs"

	req := httptest.NewRequest("GET", "/accounts/oauth/pending?u="+url.QueryEscape(authURL), nil)
	got := oauthShowAuthURL(req)
	if got != authURL {
		t.Fatalf("oauth show URL decoded inner query:\n got: %s\nwant: %s", got, authURL)
	}
	if strings.Contains(got, "scope=openid profile") || strings.Contains(got, "redirect_uri=http://") {
		t.Fatalf("inner OAuth query must remain percent-encoded, got: %s", got)
	}
}

func TestOAuthAuthURLTemplatePreservesPercentEscapes(t *testing.T) {
	authURL := "https://auth.openai.com/oauth/authorize?" +
		"redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback" +
		"&scope=openid%20profile%20email%20offline_access" +
		"&state=state"

	tpl := template.Must(template.New("oauth-link").Parse(`<a href="{{.}}">{{.}}</a>`))
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, authURL); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback") {
		t.Fatalf("template lost redirect_uri percent escapes: %s", out)
	}
	if !strings.Contains(out, "scope=openid%20profile%20email%20offline_access") {
		t.Fatalf("template lost scope percent escapes: %s", out)
	}
	if strings.Contains(out, "redirect_uri=http://") || strings.Contains(out, "%253A%252F%252F") {
		t.Fatalf("template decoded or double-encoded inner URL: %s", out)
	}
}

func TestOAuthStartWebSessionModeUsesEnrollmentFlowWithoutOAuthManager(t *testing.T) {
	s := &Server{enroll: enrollment.New()}
	form := url.Values{}
	form.Set("login_mode", "web_session")
	form.Set("tenant_id", "default")
	form.Set("note", "web session account")

	req := httptest.NewRequest(http.MethodPost, "/accounts/oauth", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	s.handleOAuthStartPost(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/accounts/oauth/web-session/") {
		t.Fatalf("redirect = %q, want web-session enrollment", loc)
	}
}

func TestOAuthWebSessionSubmitPersistsSessionAndCookies(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pool.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	s := &Server{
		enroll: enrollment.New(),
		deps: Deps{
			Store: st,
			Sched: scheduler.New(config.Scheduler{}),
		},
	}
	pending := s.enroll.Create("default", "chatgpt", "", "web session")
	session := `{"accessToken":"access-1","expires":"2099-01-01T00:00:00Z","account":{"id":"chatgpt-account","planType":"plus"},"user":{"email":"web@example.com"}}`
	form := url.Values{}
	form.Set("session", session)
	form.Set("cookies", "__Secure-next-auth.session-token=cookie-1; other=keep")
	form.Set("ua", "codex-web-session-test")

	req := httptest.NewRequest(http.MethodPost, "/accounts/oauth/web-session/"+pending.ID+"/submit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", pending.ID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()

	s.handleOAuthWebSessionSubmit(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	got, ok := s.enroll.Get(pending.ID)
	if !ok || got.State != enrollment.StateCompleted || got.AccountID == "" {
		t.Fatalf("pending not completed: %+v ok=%v", got, ok)
	}
	acc, err := st.GetAccount(t.Context(), got.AccountID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if acc.Provider != "chatgpt" || acc.Email != "web@example.com" || acc.UA != "codex-web-session-test" {
		t.Fatalf("unexpected account: %+v", acc)
	}
	sec, err := st.GetAccountSecret(t.Context(), got.AccountID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if sec.SessionToken != session || string(sec.Cookies) != "__Secure-next-auth.session-token=cookie-1; other=keep" {
		t.Fatalf("unexpected secret: %+v", sec)
	}
}

func TestOAuthWebSessionSubmitNormalizesChromeApplicationCookieTable(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pool.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	s := &Server{
		enroll: enrollment.New(),
		deps: Deps{
			Store: st,
			Sched: scheduler.New(config.Scheduler{}),
		},
	}
	pending := s.enroll.Create("default", "chatgpt", "", "web session")
	session := `{"accessToken":"access-1","expires":"2099-01-01T00:00:00Z","account":{"id":"chatgpt-account","planType":"plus"},"user":{"email":"web@example.com"}}`
	cookies := strings.Join([]string{
		"Name\tValue\tDomain\tPath\tExpires\tSize\tHttpOnly\tSecure\tSameSite\tPriority",
		"__Secure-next-auth.session-token.0\tpart0\t.chatgpt.com\t/\t2026-08-24T05:54:55.225Z\t3967\t✓\t✓\tLax\tMedium",
		"__Secure-next-auth.session-token.1\tpart1\t.chatgpt.com\t/\t2026-08-24T05:54:55.227Z\t77\t✓\t✓\tLax\tMedium",
		"cf_clearance\tclear-token\t.chatgpt.com\t/\t2027-05-26T05:51:27.062Z\t417\t✓\t✓\tNone\tMedium",
	}, "\n")
	form := url.Values{}
	form.Set("session", session)
	form.Set("cookies", cookies)

	req := httptest.NewRequest(http.MethodPost, "/accounts/oauth/web-session/"+pending.ID+"/submit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", pending.ID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()

	s.handleOAuthWebSessionSubmit(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	got, ok := s.enroll.Get(pending.ID)
	if !ok || got.AccountID == "" {
		t.Fatalf("pending not completed: %+v ok=%v", got, ok)
	}
	sec, err := st.GetAccountSecret(t.Context(), got.AccountID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	stored := string(sec.Cookies)
	for _, want := range []string{
		"__Secure-next-auth.session-token.0=part0",
		"__Secure-next-auth.session-token.1=part1",
		"cf_clearance=clear-token",
	} {
		if !strings.Contains(stored, want) {
			t.Fatalf("stored cookies = %q, missing %q", stored, want)
		}
	}
	if strings.Contains(stored, "\t") || strings.Contains(stored, "Name=Value") {
		t.Fatalf("stored cookies were not normalized: %q", stored)
	}
}

func TestOAuthWebSessionShowRendersCapturePage(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "pool.db"), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	s := New(Deps{
		Cfg:   &config.Root{},
		Store: st,
		Sched: scheduler.New(config.Scheduler{}),
	})
	s.enroll = enrollment.New()
	pending := s.enroll.Create("default", "chatgpt", "", "web session")

	req := httptest.NewRequest(http.MethodGet, "/accounts/oauth/web-session/"+pending.ID, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", pending.ID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()

	s.handleOAuthWebSessionShow(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"ChatGPT Web Session 导入",
		"/accounts/oauth/web-session/" + pending.ID + "/submit",
		"__Secure-next-auth.session-token",
		"/api/auth/session",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("rendered page missing %q: %s", want, body)
		}
	}
}
