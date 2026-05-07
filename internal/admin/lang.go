package admin

import (
	"net/http"

	"github.com/llm-pool/gateway/internal/i18n"
)

const langCookie = "llm_pool_lang"

func langFromReq(r *http.Request) i18n.Lang {
	if c, err := r.Cookie(langCookie); err == nil && c.Value != "" {
		return i18n.Normalize(c.Value)
	}
	if q := r.URL.Query().Get("lang"); q != "" {
		return i18n.Normalize(q)
	}
	return i18n.CN
}

// t is a per-request translator for use in Go-side messages (errors etc).
func (s *Server) t(r *http.Request, key string) string {
	return i18n.T(langFromReq(r), key)
}

func (s *Server) handleSetLang(w http.ResponseWriter, r *http.Request) {
	l := r.URL.Query().Get("l")
	if l == "" {
		l = "cn"
	}
	http.SetCookie(w, &http.Cookie{
		Name: langCookie, Value: string(i18n.Normalize(l)), Path: "/",
		MaxAge: 365 * 24 * 3600,
	})
	back := r.URL.Query().Get("back")
	if back == "" {
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
