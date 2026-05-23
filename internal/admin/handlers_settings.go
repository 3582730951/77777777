package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/store"
)

func (s *Server) handleTokenOptimizerSettings(w http.ResponseWriter, r *http.Request) {
	opt, source, err := s.currentTokenOptimizer(r)
	data := map[string]any{
		"Active": "settings",
		"Title":  "系统设置",
		"Opt":    opt,
		"Source": source,
		"Saved":  r.URL.Query().Get("saved") == "1",
	}
	if err != nil {
		data["Error"] = err.Error()
	}
	s.render(w, r, "settings_token_optimizer.html", data)
}

func (s *Server) handleTokenOptimizerSettingsPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderTokenOptimizerError(w, r, config.NormalizeTokenOptimizer(s.deps.Cfg.TokenOptimizer), err)
		return
	}
	mode, ok := config.NormalizeTokenOptimizerMode(r.FormValue("mode"))
	if !ok {
		s.renderTokenOptimizerError(w, r, config.NormalizeTokenOptimizer(s.deps.Cfg.TokenOptimizer), fmt.Errorf("invalid mode"))
		return
	}
	current := config.NormalizeTokenOptimizer(s.deps.Cfg.TokenOptimizer)
	opt := config.TokenOptimizer{Mode: mode}
	var err error
	if opt.MinToolOutputBytes, err = parsePositiveIntForm(r, "min_tool_output_bytes", current.MinToolOutputBytes); err != nil {
		s.renderTokenOptimizerError(w, r, current, err)
		return
	}
	if opt.MaxOptimizedToolOutputBytes, err = parsePositiveIntForm(r, "max_optimized_tool_output_bytes", current.MaxOptimizedToolOutputBytes); err != nil {
		s.renderTokenOptimizerError(w, r, current, err)
		return
	}
	if opt.HeadLines, err = parsePositiveIntForm(r, "head_lines", current.HeadLines); err != nil {
		s.renderTokenOptimizerError(w, r, current, err)
		return
	}
	if opt.TailLines, err = parsePositiveIntForm(r, "tail_lines", current.TailLines); err != nil {
		s.renderTokenOptimizerError(w, r, current, err)
		return
	}
	if opt.ErrorContextLines, err = parsePositiveIntForm(r, "error_context_lines", current.ErrorContextLines); err != nil {
		s.renderTokenOptimizerError(w, r, current, err)
		return
	}
	opt = config.NormalizeTokenOptimizer(opt)

	payload, err := json.Marshal(opt)
	if err != nil {
		s.renderTokenOptimizerError(w, r, current, err)
		return
	}
	if err := s.deps.Store.SetSetting(r.Context(), store.SettingTokenOptimizer, string(payload)); err != nil {
		s.renderTokenOptimizerError(w, r, current, err)
		return
	}
	s.deps.Cfg.TokenOptimizer = opt
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "settings", "", "", "token optimizer updated: "+opt.Mode)
	}
	http.Redirect(w, r, "/settings/token-optimizer?saved=1", http.StatusSeeOther)
}

func (s *Server) handleNetworkSettings(w http.ResponseWriter, r *http.Request) {
	network, source, err := s.currentNetworkShaper(r)
	data := map[string]any{
		"Active":  "settings-network",
		"Title":   "网络限速",
		"Network": network,
		"Source":  source,
		"Saved":   r.URL.Query().Get("saved") == "1",
	}
	if err != nil {
		data["Error"] = err.Error()
	}
	s.render(w, r, "settings_network.html", data)
}

func (s *Server) handleNetworkSettingsPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderNetworkSettingsError(w, r, config.NetworkShaperFromServer(s.deps.Cfg.Server), err)
		return
	}
	current := config.NetworkShaperFromServer(s.deps.Cfg.Server)
	network := config.NetworkShaper{}
	var err error
	if network.NetworkIngressBytesPerSec, err = parseNonNegativeInt64Form(r, "network_ingress_bytes_per_sec", current.NetworkIngressBytesPerSec); err != nil {
		s.renderNetworkSettingsError(w, r, current, err)
		return
	}
	if network.NetworkEgressBytesPerSec, err = parseNonNegativeInt64Form(r, "network_egress_bytes_per_sec", current.NetworkEgressBytesPerSec); err != nil {
		s.renderNetworkSettingsError(w, r, current, err)
		return
	}
	if network.NetworkBurstBytes, err = parseNonNegativeInt64Form(r, "network_burst_bytes", current.NetworkBurstBytes); err != nil {
		s.renderNetworkSettingsError(w, r, current, err)
		return
	}
	network = config.NormalizeNetworkShaper(network)

	payload, err := json.Marshal(network)
	if err != nil {
		s.renderNetworkSettingsError(w, r, current, err)
		return
	}
	if err := s.deps.Store.SetSetting(r.Context(), store.SettingNetworkShaper, string(payload)); err != nil {
		s.renderNetworkSettingsError(w, r, current, err)
		return
	}
	config.ApplyNetworkShaperToServer(network, &s.deps.Cfg.Server)
	if s.deps.NetShaper != nil {
		s.deps.NetShaper.ApplyNetworkShapingConfig(s.deps.Cfg.Server)
	}
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "settings", "", "", fmt.Sprintf("network shaper updated: ingress=%d egress=%d burst=%d",
			network.NetworkIngressBytesPerSec, network.NetworkEgressBytesPerSec, network.NetworkBurstBytes))
	}
	http.Redirect(w, r, "/settings/network?saved=1", http.StatusSeeOther)
}

func (s *Server) handleProxyPoolSettings(w http.ResponseWriter, r *http.Request) {
	pool, source, err := s.currentProxyPool(r)
	data := s.proxyPoolTemplateData(pool, source)
	data["Saved"] = r.URL.Query().Get("saved") == "1"
	if err != nil {
		data["Error"] = err.Error()
	}
	s.render(w, r, "settings_proxy_pool.html", data)
}

func (s *Server) handleProxyPoolSettingsPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderProxyPoolSettingsError(w, r, config.NormalizeProxyPool(s.deps.Cfg.ProxyPool), err)
		return
	}
	current := config.NormalizeProxyPool(s.deps.Cfg.ProxyPool)
	pool := config.ProxyPool{}
	enabled := r.FormValue("enabled") == "on"
	pool.Enabled = &enabled
	if raw := strings.TrimSpace(r.FormValue("sticky_window")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			s.renderProxyPoolSettingsError(w, r, current, fmt.Errorf("sticky_window must be a positive Go duration, e.g. 24h"))
			return
		}
		pool.StickyWindow = d
	}
	geoIPEnabled := r.FormValue("geoip_enabled") == "on"
	pool.GeoIP.Enabled = &geoIPEnabled
	pool.GeoIP.Endpoint = strings.TrimSpace(r.FormValue("geoip_endpoint"))
	if raw := strings.TrimSpace(r.FormValue("geoip_timeout")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			s.renderProxyPoolSettingsError(w, r, current, fmt.Errorf("geoip_timeout must be a positive Go duration, e.g. 10s"))
			return
		}
		pool.GeoIP.Timeout = d
	}
	proxiesText := strings.TrimSpace(r.FormValue("proxies_json"))
	if proxiesText != "" {
		if err := json.Unmarshal([]byte(proxiesText), &pool.Proxies); err != nil {
			s.renderProxyPoolSettingsError(w, r, current, fmt.Errorf("parse proxies_json: %w", err))
			return
		}
	}
	pool.AccountBindings = parseProxyBindings(r.FormValue("account_bindings"))
	pool = config.NormalizeProxyPool(pool)

	payload, err := json.Marshal(pool)
	if err != nil {
		s.renderProxyPoolSettingsError(w, r, current, err)
		return
	}
	if err := s.deps.Store.SetSetting(r.Context(), store.SettingProxyPool, string(payload)); err != nil {
		s.renderProxyPoolSettingsError(w, r, current, err)
		return
	}
	s.deps.Cfg.ProxyPool = pool
	if s.deps.ProxyPool != nil {
		s.deps.ProxyPool.UpdateConfig(pool)
	}
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "settings", "", "", fmt.Sprintf("proxy pool updated: proxies=%d", len(pool.Proxies)))
	}
	http.Redirect(w, r, "/settings/proxy-pool?saved=1", http.StatusSeeOther)
}

func (s *Server) currentTokenOptimizer(r *http.Request) (config.TokenOptimizer, string, error) {
	opt := config.NormalizeTokenOptimizer(s.deps.Cfg.TokenOptimizer)
	value, ok, err := s.deps.Store.GetSetting(r.Context(), store.SettingTokenOptimizer)
	if err != nil {
		return opt, "memory", err
	}
	if !ok || strings.TrimSpace(value) == "" {
		return opt, "config.yaml", nil
	}
	var stored config.TokenOptimizer
	if err := json.Unmarshal([]byte(value), &stored); err != nil {
		return opt, "memory", err
	}
	opt = config.NormalizeTokenOptimizer(stored)
	s.deps.Cfg.TokenOptimizer = opt
	return opt, "admin", nil
}

func (s *Server) currentNetworkShaper(r *http.Request) (config.NetworkShaper, string, error) {
	network := config.NetworkShaperFromServer(s.deps.Cfg.Server)
	value, ok, err := s.deps.Store.GetSetting(r.Context(), store.SettingNetworkShaper)
	if err != nil {
		return network, "memory", err
	}
	if !ok || strings.TrimSpace(value) == "" {
		return network, "config.yaml", nil
	}
	var stored config.NetworkShaper
	if err := json.Unmarshal([]byte(value), &stored); err != nil {
		return network, "memory", err
	}
	network = config.NormalizeNetworkShaper(stored)
	config.ApplyNetworkShaperToServer(network, &s.deps.Cfg.Server)
	if s.deps.NetShaper != nil {
		s.deps.NetShaper.ApplyNetworkShapingConfig(s.deps.Cfg.Server)
	}
	return network, "admin", nil
}

func (s *Server) currentProxyPool(r *http.Request) (config.ProxyPool, string, error) {
	pool := config.NormalizeProxyPool(s.deps.Cfg.ProxyPool)
	value, ok, err := s.deps.Store.GetSetting(r.Context(), store.SettingProxyPool)
	if err != nil {
		return pool, "memory", err
	}
	if !ok || strings.TrimSpace(value) == "" {
		return pool, "config.yaml", nil
	}
	var stored config.ProxyPool
	if err := json.Unmarshal([]byte(value), &stored); err != nil {
		return pool, "memory", err
	}
	pool = config.NormalizeProxyPool(stored)
	s.deps.Cfg.ProxyPool = pool
	if s.deps.ProxyPool != nil {
		s.deps.ProxyPool.UpdateConfig(pool)
	}
	return pool, "admin", nil
}

func (s *Server) renderTokenOptimizerError(w http.ResponseWriter, r *http.Request, opt config.TokenOptimizer, err error) {
	s.render(w, r, "settings_token_optimizer.html", map[string]any{
		"Active": "settings",
		"Title":  "系统设置",
		"Opt":    config.NormalizeTokenOptimizer(opt),
		"Source": "memory",
		"Error":  err.Error(),
	})
}

func (s *Server) renderNetworkSettingsError(w http.ResponseWriter, r *http.Request, network config.NetworkShaper, err error) {
	s.render(w, r, "settings_network.html", map[string]any{
		"Active":  "settings-network",
		"Title":   "网络限速",
		"Network": config.NormalizeNetworkShaper(network),
		"Source":  "memory",
		"Error":   err.Error(),
	})
}

func (s *Server) renderProxyPoolSettingsError(w http.ResponseWriter, r *http.Request, pool config.ProxyPool, err error) {
	data := s.proxyPoolTemplateData(config.NormalizeProxyPool(pool), "memory")
	data["Error"] = err.Error()
	s.render(w, r, "settings_proxy_pool.html", data)
}

func (s *Server) proxyPoolTemplateData(pool config.ProxyPool, source string) map[string]any {
	pool = config.NormalizeProxyPool(pool)
	proxiesJSON, _ := json.MarshalIndent(pool.Proxies, "", "  ")
	return map[string]any{
		"Active":          "settings-proxy-pool",
		"Title":           "出口代理池",
		"Pool":            pool,
		"Enabled":         enabledForProxyPool(pool),
		"GeoIPEnabled":    enabledPtr(pool.GeoIP.Enabled, true),
		"StickyWindow":    pool.StickyWindow.String(),
		"GeoIPTimeout":    pool.GeoIP.Timeout.String(),
		"ProxiesJSON":     string(proxiesJSON),
		"AccountBindings": formatProxyBindings(pool.AccountBindings),
		"Source":          source,
	}
}

func enabledForProxyPool(pool config.ProxyPool) bool {
	if pool.Enabled != nil {
		return *pool.Enabled
	}
	return len(pool.Proxies) > 0
}

func enabledPtr(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

func parseProxyBindings(raw string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var k, v string
		if left, right, ok := strings.Cut(line, "="); ok {
			k, v = left, right
		} else {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				k, v = fields[0], fields[1]
			}
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k != "" && v != "" {
			out[k] = v
		}
	}
	return out
}

func formatProxyBindings(bindings map[string]string) string {
	if len(bindings) == 0 {
		return ""
	}
	keys := make([]string, 0, len(bindings))
	for key := range bindings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(bindings[key])
	}
	return b.String()
}

func parsePositiveIntForm(r *http.Request, name string, fallback int) (int, error) {
	raw := strings.TrimSpace(r.FormValue(name))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return n, nil
}

func parseNonNegativeInt64Form(r *http.Request, name string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(r.FormValue(name))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return n, nil
}
