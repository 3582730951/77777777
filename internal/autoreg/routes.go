package autoreg

import (
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/go-chi/chi/v5"
)

func MountRoutes(r chi.Router, proxyTarget string) {
	target, err := url.Parse(proxyTarget)
	if err != nil {
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1 // disable buffering for SSE

	r.Route("/api/autoreg", func(r chi.Router) {
		r.HandleFunc("/*", func(w http.ResponseWriter, req *http.Request) {
			proxy.ServeHTTP(w, req)
		})
	})
}
