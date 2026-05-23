package proxypool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/domain"
)

type AffinityStore interface {
	GetProxyAffinity(ctx context.Context, accountID string) (country string, windowStart time.Time, proxyID string, ok bool, err error)
	UpsertProxyAffinity(ctx context.Context, accountID, country, proxyID string, windowStart time.Time) error
}

type CountryResolver interface {
	Country(ctx context.Context, proxyURL string) (string, error)
}

type Manager struct {
	mu           sync.Mutex
	cfg          config.ProxyPool
	store        AffinityStore
	resolver     CountryResolver
	now          func() time.Time
	rr           map[string]int
	countryCache map[string]string
	memAffinity  map[string]affinity
}

type Proxy struct {
	ID      string
	URL     string
	Country string
	Weight  int
}

type Selection struct {
	Proxy       *Proxy
	Direct      bool
	Reason      string
	WindowStart time.Time
}

type affinity struct {
	country     string
	windowStart time.Time
	proxyID     string
}

func New(cfg config.ProxyPool, store AffinityStore) *Manager {
	cfg = config.NormalizeProxyPool(cfg)
	return &Manager{
		cfg:          cfg,
		store:        store,
		resolver:     NewDefaultCountryResolver(cfg.GeoIP),
		now:          time.Now,
		rr:           map[string]int{},
		countryCache: map[string]string{},
		memAffinity:  map[string]affinity{},
	}
}

func NewWithResolver(cfg config.ProxyPool, store AffinityStore, resolver CountryResolver) *Manager {
	m := New(cfg, store)
	if resolver != nil {
		m.resolver = resolver
	}
	return m
}

func (m *Manager) UpdateConfig(cfg config.ProxyPool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = config.NormalizeProxyPool(cfg)
	m.resolver = NewDefaultCountryResolver(m.cfg.GeoIP)
	m.rr = map[string]int{}
	m.countryCache = map[string]string{}
}

func (m *Manager) Config() config.ProxyPool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

func (m *Manager) Resolve(ctx context.Context, acc *domain.Account) (Selection, error) {
	if m == nil || acc == nil {
		return Selection{Direct: true, Reason: "direct"}, nil
	}
	cfg := m.Config()
	target := strings.TrimSpace(acc.Proxy)
	proxies, err := m.activeProxies(ctx, cfg)
	if err != nil {
		return Selection{}, err
	}
	if target != "" {
		return m.resolveExplicitTarget(ctx, cfg, acc.ID, target, proxies, "account_proxy")
	}
	if proxyPoolEnabled(cfg) && len(proxies) > 0 {
		if binding := strings.TrimSpace(cfg.AccountBindings[acc.ID]); binding != "" {
			return m.resolveExplicitTarget(ctx, cfg, acc.ID, binding, proxies, "account_binding")
		}
	}
	if !proxyPoolEnabled(cfg) || len(proxies) == 0 {
		return Selection{Direct: true, Reason: "direct"}, nil
	}
	now := m.nowTime()
	if len(proxies) == 1 {
		proxy := proxies[0]
		if err := m.saveAffinity(ctx, acc.ID, proxy.Country, proxy.ID, now); err != nil {
			return Selection{}, err
		}
		return Selection{Proxy: &proxy, Reason: "single_proxy", WindowStart: now}, nil
	}
	if aff, ok, err := m.loadAffinity(ctx, acc.ID); err != nil {
		return Selection{}, err
	} else if ok && aff.country != "" && now.Sub(aff.windowStart) < cfg.StickyWindow {
		candidates := filterByCountry(proxies, aff.country)
		if len(candidates) > 0 {
			proxy := m.chooseWeighted(candidates, "country:"+aff.country)
			if err := m.saveAffinity(ctx, acc.ID, proxy.Country, proxy.ID, aff.windowStart); err != nil {
				return Selection{}, err
			}
			return Selection{Proxy: &proxy, Reason: "sticky_country", WindowStart: aff.windowStart}, nil
		}
	}
	proxy := m.chooseWeighted(proxies, "all")
	if err := m.saveAffinity(ctx, acc.ID, proxy.Country, proxy.ID, now); err != nil {
		return Selection{}, err
	}
	return Selection{Proxy: &proxy, Reason: "balanced", WindowStart: now}, nil
}

func (m *Manager) resolveExplicitTarget(ctx context.Context, cfg config.ProxyPool, accountID, target string, proxies []Proxy, reason string) (Selection, error) {
	if proxyURL(target) {
		proxy, err := m.proxyFromURL(ctx, cfg, target)
		if err != nil {
			return Selection{}, err
		}
		now := m.nowTime()
		if err := m.saveAffinity(ctx, accountID, proxy.Country, proxy.ID, now); err != nil {
			return Selection{}, err
		}
		return Selection{Proxy: &proxy, Reason: reason + "_url", WindowStart: now}, nil
	}
	if proxy, ok := proxyByID(proxies, target); ok {
		now := m.nowTime()
		if err := m.saveAffinity(ctx, accountID, proxy.Country, proxy.ID, now); err != nil {
			return Selection{}, err
		}
		return Selection{Proxy: &proxy, Reason: reason + "_id", WindowStart: now}, nil
	}
	country, ok := countryTarget(target)
	if ok {
		candidates := filterByCountry(proxies, country)
		if len(candidates) == 0 {
			return Selection{}, fmt.Errorf("proxy pool: no enabled proxy for country %s", country)
		}
		proxy := m.chooseWeighted(candidates, "country:"+country)
		now := m.nowTime()
		if err := m.saveAffinity(ctx, accountID, proxy.Country, proxy.ID, now); err != nil {
			return Selection{}, err
		}
		return Selection{Proxy: &proxy, Reason: reason + "_country", WindowStart: now}, nil
	}
	return Selection{}, fmt.Errorf("proxy pool: unknown proxy target %q", target)
}

func (m *Manager) activeProxies(ctx context.Context, cfg config.ProxyPool) ([]Proxy, error) {
	if !proxyPoolEnabled(cfg) {
		return nil, nil
	}
	out := make([]Proxy, 0, len(cfg.Proxies))
	for _, p := range cfg.Proxies {
		if p.URL == "" || !enabledPtr(p.Enabled, true) {
			continue
		}
		country := normalizeCountry(p.Country)
		if country == "" {
			var err error
			country, err = m.resolveCountry(ctx, cfg, p.ID, p.URL)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, Proxy{
			ID:      p.ID,
			URL:     p.URL,
			Country: country,
			Weight:  p.Weight,
		})
	}
	return out, nil
}

func (m *Manager) proxyFromURL(ctx context.Context, cfg config.ProxyPool, raw string) (Proxy, error) {
	country, err := m.resolveCountry(ctx, cfg, "url:"+raw, raw)
	if err != nil {
		return Proxy{}, err
	}
	return Proxy{
		ID:      "url:" + raw,
		URL:     raw,
		Country: country,
		Weight:  1,
	}, nil
}

func (m *Manager) resolveCountry(ctx context.Context, cfg config.ProxyPool, cacheKey, proxyRaw string) (string, error) {
	cacheKey = strings.TrimSpace(cacheKey)
	m.mu.Lock()
	if country := m.countryCache[cacheKey]; country != "" {
		m.mu.Unlock()
		return country, nil
	}
	resolver := m.resolver
	m.mu.Unlock()

	var country string
	var err error
	if geoIPEnabled(cfg.GeoIP.Enabled) {
		if resolver == nil {
			return "", errors.New("proxy pool: geoip resolver not configured")
		}
		country, err = resolver.Country(ctx, proxyRaw)
		if err != nil {
			return "", fmt.Errorf("proxy pool: resolve country for %s: %w", cacheKey, err)
		}
	}
	country = normalizeCountry(country)
	if country == "" {
		country = "ZZ"
	}
	m.mu.Lock()
	m.countryCache[cacheKey] = country
	m.mu.Unlock()
	return country, nil
}

func (m *Manager) loadAffinity(ctx context.Context, accountID string) (affinity, bool, error) {
	if accountID == "" {
		return affinity{}, false, nil
	}
	if m.store != nil {
		country, start, proxyID, ok, err := m.store.GetProxyAffinity(ctx, accountID)
		if err != nil {
			return affinity{}, false, fmt.Errorf("proxy affinity read: %w", err)
		}
		if !ok {
			return affinity{}, false, nil
		}
		return affinity{country: normalizeCountry(country), windowStart: start, proxyID: proxyID}, true, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	aff, ok := m.memAffinity[accountID]
	return aff, ok, nil
}

func (m *Manager) saveAffinity(ctx context.Context, accountID, country, proxyID string, windowStart time.Time) error {
	if accountID == "" || country == "" || windowStart.IsZero() {
		return nil
	}
	country = normalizeCountry(country)
	if m.store != nil {
		return m.store.UpsertProxyAffinity(ctx, accountID, country, proxyID, windowStart)
	}
	m.mu.Lock()
	m.memAffinity[accountID] = affinity{country: country, windowStart: windowStart, proxyID: proxyID}
	m.mu.Unlock()
	return nil
}

func (m *Manager) chooseWeighted(proxies []Proxy, key string) Proxy {
	if len(proxies) == 1 {
		return proxies[0]
	}
	total := 0
	for _, proxy := range proxies {
		weight := proxy.Weight
		if weight <= 0 {
			weight = 1
		}
		total += weight
	}
	if total <= 0 {
		return proxies[0]
	}
	m.mu.Lock()
	idx := m.rr[key] % total
	m.rr[key]++
	m.mu.Unlock()
	for _, proxy := range proxies {
		weight := proxy.Weight
		if weight <= 0 {
			weight = 1
		}
		if idx < weight {
			return proxy
		}
		idx -= weight
	}
	return proxies[len(proxies)-1]
}

func (m *Manager) nowTime() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.now == nil {
		return time.Now()
	}
	return m.now()
}

func proxyPoolEnabled(cfg config.ProxyPool) bool {
	if !enabledPtr(cfg.Enabled, true) {
		return false
	}
	return len(cfg.Proxies) > 0
}

func filterByCountry(proxies []Proxy, country string) []Proxy {
	country = normalizeCountry(country)
	out := make([]Proxy, 0, len(proxies))
	for _, proxy := range proxies {
		if normalizeCountry(proxy.Country) == country {
			out = append(out, proxy)
		}
	}
	return out
}

func proxyByID(proxies []Proxy, id string) (Proxy, bool) {
	id = strings.TrimSpace(id)
	for _, proxy := range proxies {
		if proxy.ID == id {
			return proxy, true
		}
	}
	return Proxy{}, false
}

func proxyURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Scheme != "" && u.Host != ""
}

func countryTarget(target string) (string, bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false
	}
	lower := strings.ToLower(target)
	if strings.HasPrefix(lower, "country:") {
		return normalizeCountry(strings.TrimSpace(target[len("country:"):])), true
	}
	country := normalizeCountry(target)
	if len(country) == 2 {
		return country, true
	}
	return "", false
}

func normalizeCountry(country string) string {
	country = strings.ToUpper(strings.TrimSpace(country))
	if country == "" {
		return ""
	}
	if len(country) > 2 {
		return country
	}
	return country
}

func enabledPtr(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

func geoIPEnabled(v *bool) bool {
	return enabledPtr(v, true)
}

type DefaultCountryResolver struct {
	Endpoint string
	Timeout  time.Duration
}

func NewDefaultCountryResolver(cfg config.GeoIPConfig) *DefaultCountryResolver {
	cfg = config.NormalizeProxyPool(config.ProxyPool{GeoIP: cfg}).GeoIP
	return &DefaultCountryResolver{Endpoint: cfg.Endpoint, Timeout: cfg.Timeout}
}

func (r *DefaultCountryResolver) Country(ctx context.Context, proxyRaw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(proxyRaw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid proxy url")
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	endpoint := strings.TrimSpace(r.Endpoint)
	if endpoint == "" {
		endpoint = "https://ipapi.co/json/"
	}
	transport := &http.Transport{
		Proxy: http.ProxyURL(u),
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json, text/plain;q=0.9")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("geoip %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if country := countryFromGeoBody(body); country != "" {
		return country, nil
	}
	return "", fmt.Errorf("geoip response missing country")
}

func countryFromGeoBody(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "{") {
		var raw map[string]any
		if json.Unmarshal(body, &raw) == nil {
			for _, key := range []string{"country_code", "countryCode", "country", "country_code2"} {
				if v, ok := raw[key].(string); ok {
					if country := normalizeCountry(v); country != "" {
						return country
					}
				}
			}
		}
	}
	if len(trimmed) >= 2 && len(trimmed) <= 3 {
		return normalizeCountry(trimmed)
	}
	return ""
}
