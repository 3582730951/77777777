package server

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
)

var responseDisclosureHeaderPrefixes = []string{
	"cf-aig-",
	"helicone-",
	"langfuse-",
	"langsmith-",
	"x-ai-gateway-",
	"x-bt-",
	"x-gateway-",
	"x-kong-",
	"x-langfuse-",
	"x-langsmith-",
	"x-litellm-",
	"x-llm-",
	"x-openrouter-",
	"x-portkey-",
}

func responseHeaderHygieneMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&hygieneResponseWriter{ResponseWriter: w}, r)
	})
}

type hygieneResponseWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *hygieneResponseWriter) WriteHeader(status int) {
	w.scrub(status)
	w.ResponseWriter.WriteHeader(status)
}

func (w *hygieneResponseWriter) Write(p []byte) (int, error) {
	w.scrub(http.StatusOK)
	return w.ResponseWriter.Write(p)
}

func (w *hygieneResponseWriter) scrub(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	scrubResponseDisclosureHeaders(w.Header(), status == http.StatusSwitchingProtocols)
}

func scrubResponseDisclosureHeaders(h http.Header, preserveUpgrade bool) {
	if !preserveUpgrade {
		for _, token := range responseConnectionHeaderTokens(h) {
			h.Del(token)
		}
		for _, k := range []string{
			"Connection",
			"Keep-Alive",
			"Proxy-Authenticate",
			"Proxy-Authorization",
			"Te",
			"Trailer",
			"Transfer-Encoding",
			"Upgrade",
		} {
			h.Del(k)
		}
	}
	for _, k := range []string{
		"Server",
		"Via",
		"Forwarded",
		"X-Forwarded-For",
		"X-Forwarded",
		"X-Forwarded-Host",
		"X-Forwarded-Proto",
		"X-Forwarded-Port",
		"X-Real-Ip",
		"X-Client-Ip",
		"True-Client-Ip",
		"Cf-Connecting-Ip",
		"Fastly-Client-Ip",
		"X-Request-Id",
		"X-Correlation-Id",
		"X-Trace-Id",
		"X-Amzn-Trace-Id",
		"X-Cloud-Trace-Context",
		"X-B3-Traceid",
		"X-B3-Spanid",
		"X-B3-Parentspanid",
		"X-B3-Sampled",
		"X-B3-Flags",
		"Traceparent",
		"Tracestate",
		"Baggage",
		"Uber-Trace-Id",
		"X-Datadog-Trace-Id",
		"X-Datadog-Parent-Id",
		"X-Datadog-Sampling-Priority",
		"X-Datadog-Origin",
		"X-Powered-By",
		"X-Aspnet-Version",
		"X-Aspnetmvc-Version",
		"X-Generator",
		"X-Backend",
		"X-Origin-Server",
		"X-Upstream",
		"X-Upstream-Addr",
		"X-Upstream-Status",
		"X-Envoy-External-Address",
		"X-Envoy-Internal",
		"X-Envoy-Original-Path",
		"X-Envoy-Decorator-Operation",
		"X-Envoy-Attempt-Count",
		"X-Envoy-Expected-Rq-Timeout-Ms",
		"X-Envoy-Upstream-Service-Time",
		"X-Kong-Proxy-Latency",
		"X-Kong-Upstream-Latency",
		"X-Nginx-Proxy",
		"X-Proxy-Cache",
		"X-Nginx-Cache",
		"X-Varnish",
		"X-Served-By",
		"X-Cache",
		"X-Cache-Hits",
		"X-Timer",
		"X-Request-Start",
		"X-Queue-Start",
		"X-Runtime",
		"X-Response-Time",
		"X-Process-Time",
	} {
		h.Del(k)
	}
	for key := range h {
		lower := strings.ToLower(key)
		for _, prefix := range responseDisclosureHeaderPrefixes {
			if strings.HasPrefix(lower, prefix) {
				h.Del(key)
				break
			}
		}
	}
}

func responseConnectionHeaderTokens(h http.Header) []string {
	var out []string
	for _, v := range h.Values("Connection") {
		for _, part := range strings.Split(v, ",") {
			token := strings.TrimSpace(part)
			if token != "" {
				out = append(out, token)
			}
		}
	}
	return out
}

func (w *hygieneResponseWriter) Flush() {
	w.scrub(http.StatusOK)
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *hygieneResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying response writer does not support hijacking")
	}
	return h.Hijack()
}

func (w *hygieneResponseWriter) Push(target string, opts *http.PushOptions) error {
	p, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return p.Push(target, opts)
}

func (w *hygieneResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(w, r)
}

func (w *hygieneResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
