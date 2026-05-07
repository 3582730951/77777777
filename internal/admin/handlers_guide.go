package admin

import "net/http"

// Guide pages: explain how to enroll real OpenAI / Claude / Gemini cookies.
func (s *Server) handleAdminGuide(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "guide.html", map[string]any{
		"Active": "guide",
		"Title":  s.t(r, "guide.title"),
		"IsAdmin": true,
	})
}

func (s *Server) handlePortalGuide(w http.ResponseWriter, r *http.Request) {
	tid := tenantFromCtx(r)
	s.render(w, r, "guide.html", map[string]any{
		"PActive":  "guide",
		"TenantID": tid,
		"IsAdmin":  false,
	})
}
