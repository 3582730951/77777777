package autoreg

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
)

func MountRoutes(r chi.Router, proxyTarget string) {
	target, err := url.Parse(proxyTarget)
	if err != nil {
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1 // disable buffering for SSE
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		out := req.Clone(req.Context())
		const prefix = "/api/autoreg"
		if strings.HasPrefix(out.URL.Path, prefix) {
			if len(out.URL.Path) > len(prefix) {
				out.URL.Path = "/api" + out.URL.Path[len(prefix):]
			} else {
				out.URL.Path = "/api"
			}
			out.URL.RawPath = ""
		}
		proxy.ServeHTTP(w, out)
	})

	r.Route("/api/autoreg", func(r chi.Router) {
		r.Handle("/", handler)
		r.Handle("/*", handler)
	})
}
