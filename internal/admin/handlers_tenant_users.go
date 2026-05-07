package admin

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

// genTenantPassword: random 16-char URL-safe.
func genTenantPassword() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	s := base64.RawURLEncoding.EncodeToString(b)
	if len(s) > 16 {
		s = s[:16]
	}
	return strings.NewReplacer("_", "K", "-", "M").Replace(s)
}

// ---- Admin tenant-user management ----

func (s *Server) handleTenantUsersList(w http.ResponseWriter, r *http.Request) {
	tid := chi.URLParam(r, "id")
	users, err := s.deps.Store.ListTenantUsers(r.Context(), tid)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSONStatus(w, 200, users)
}

func (s *Server) handleTenantUserCreatePost(w http.ResponseWriter, r *http.Request) {
	tid := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	if username == "" {
		http.Error(w, "username required", 400)
		return
	}
	password := strings.TrimSpace(r.FormValue("password"))
	if password == "" {
		password = genTenantPassword()
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := s.deps.Store.CreateTenantUser(r.Context(), username, tid, string(hash)); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "tenant_user", "", "", "user "+username+" added to tenant "+tid)
	}
	tenantUserFlashStore.set(username, password)
	http.Redirect(w, r, "/tenants?flash_user="+url.QueryEscape(username), http.StatusSeeOther)
}

func (s *Server) handleTenantUserResetPost(w http.ResponseWriter, r *http.Request) {
	tid := chi.URLParam(r, "id")
	username := chi.URLParam(r, "username")
	pw := strings.TrimSpace(r.FormValue("password"))
	if pw == "" {
		pw = genTenantPassword()
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_ = s.deps.Store.CreateTenantUser(r.Context(), username, tid, string(hash))
	if s.crud.Audit != nil {
		s.crud.Audit.Log("warn", "tenant_user", "", "", "password reset for "+username)
	}
	tenantUserFlashStore.set(username, pw)
	http.Redirect(w, r, "/tenants?flash_user="+url.QueryEscape(username), http.StatusSeeOther)
}

func (s *Server) handleTenantUserDeletePost(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	_ = s.deps.Store.DeleteTenantUser(r.Context(), username)
	if s.crud.Audit != nil {
		s.crud.Audit.Log("warn", "tenant_user", "", "", "user "+username+" deleted")
	}
	http.Redirect(w, r, "/tenants", http.StatusSeeOther)
}

// ---- Portal login: now via username + password ----

func (s *Server) handlePortalLoginV2(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	if username == "" || password == "" {
		s.render(w, r, "portal_login.html", map[string]any{
			"Error": s.t(r, "portal.login_required"),
			"Lang":  langFromReq(r),
		})
		return
	}
	tid, hash, ok, _ := s.deps.Store.LookupTenantUser(r.Context(), username)
	if !ok {
		s.render(w, r, "portal_login.html", map[string]any{
			"Error": s.t(r, "portal.login_invalid"),
			"Lang":  langFromReq(r),
		})
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		s.render(w, r, "portal_login.html", map[string]any{
			"Error": s.t(r, "portal.login_invalid"),
			"Lang":  langFromReq(r),
		})
		return
	}
	tok := generateToken()
	tenantSessionStore.set(tok, tid)
	http.SetCookie(w, &http.Cookie{
		Name: tenantSessionCookie, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 12 * 3600,
	})
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "portal_login", "", "", "tenant_user="+username+" tenant="+tid)
	}
	http.Redirect(w, r, "/portal", http.StatusSeeOther)
	_ = time.Now
}

// flash store for showing tenant-user passwords once.
var tenantUserFlashStore = &flashStore{m: map[string]flashEntry{}}

// ---- Portal self-registration ----

func (s *Server) handlePortalRegisterPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "portal_register.html", map[string]any{})
}

func (s *Server) handlePortalRegister(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := strings.TrimSpace(r.FormValue("password"))
	confirm := strings.TrimSpace(r.FormValue("confirm"))
	inviteCode := strings.TrimSpace(r.FormValue("invite_code"))

	renderErr := func(msg string) {
		s.render(w, r, "portal_register.html", map[string]any{"Error": msg, "Username": username})
	}

	if username == "" || password == "" {
		renderErr("用户名和密码不能为空")
		return
	}
	if len(username) < 3 {
		renderErr("用户名至少 3 个字符")
		return
	}
	if password != confirm {
		renderErr("两次密码不一致")
		return
	}
	if len(password) < 6 {
		renderErr("密码至少 6 个字符")
		return
	}

	// Check invite code if configured
	cfgCode := s.deps.Cfg.Portal.InviteCode
	if cfgCode != "" && inviteCode != cfgCode {
		renderErr("邀请码错误")
		return
	}

	// Default tenant = "default"
	tenantID := "default"
	if s.deps.Cfg.Portal.DefaultTenant != "" {
		tenantID = s.deps.Cfg.Portal.DefaultTenant
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := s.deps.Store.CreateTenantUser(r.Context(), username, tenantID, string(hash)); err != nil {
		renderErr("用户名已存在，请换一个")
		return
	}

	// Auto login
	tok := generateToken()
	tenantSessionStore.set(tok, tenantID)
	http.SetCookie(w, &http.Cookie{
		Name: tenantSessionCookie, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 12 * 3600,
	})
	http.Redirect(w, r, "/portal", http.StatusSeeOther)
}
