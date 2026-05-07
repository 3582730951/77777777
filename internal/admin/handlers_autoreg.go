package admin

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

//go:embed autoreg_spa/*
var autoregSPA embed.FS

func (s *Server) autoregSPAHandler() http.Handler {
	sub, _ := fs.Sub(autoregSPA, "autoreg_spa")
	indexBytes, _ := fs.ReadFile(sub, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/autoreg")
		path = strings.TrimPrefix(path, "/")

		if path == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(indexBytes)
			return
		}

		// Try to open file
		f, err := sub.Open(path)
		if err == nil {
			defer f.Close()
			stat, _ := f.Stat()
			if !stat.IsDir() {
				ct := "application/octet-stream"
				switch {
				case strings.HasSuffix(path, ".css"):
					ct = "text/css; charset=utf-8"
				case strings.HasSuffix(path, ".js"):
					ct = "application/javascript; charset=utf-8"
				case strings.HasSuffix(path, ".svg"):
					ct = "image/svg+xml"
				case strings.HasSuffix(path, ".html"):
					ct = "text/html; charset=utf-8"
				case strings.HasSuffix(path, ".json"):
					ct = "application/json"
				case strings.HasSuffix(path, ".png"):
					ct = "image/png"
				}
				w.Header().Set("Content-Type", ct)
				if strings.HasPrefix(path, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				io.Copy(w, f)
				return
			}
		}

		// SPA fallback
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(indexBytes)
	})
}

func (s *Server) autoregProxyHandler() http.Handler {
	listen := "127.0.0.1:9900"
	if s.deps.Cfg != nil && s.deps.Cfg.AutoReg.Listen != "" {
		listen = s.deps.Cfg.AutoReg.Listen
	}
	target, _ := url.Parse("http://" + listen)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/api/autoreg"
		if len(r.URL.Path) > len(prefix) {
			r.URL.Path = "/api" + r.URL.Path[len(prefix):]
		} else {
			r.URL.Path = "/api"
		}
		r.URL.RawPath = ""
		proxy.ServeHTTP(w, r)
	})
}

func (s *Server) handleKiroGateway(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "kiro_gateway.html", map[string]any{
		"Active": "kiro-gateway",
		"Title":  "Kiro Gateway",
	})
}
