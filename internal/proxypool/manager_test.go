package proxypool

import (
	"context"
	"testing"
	"time"

	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/domain"
)

type fakeCountryResolver struct {
	country string
	calls   int
}

func (f *fakeCountryResolver) Country(context.Context, string) (string, error) {
	f.calls++
	return f.country, nil
}

func boolPtr(v bool) *bool { return &v }

func TestResolveNoPoolDirect(t *testing.T) {
	m := NewWithResolver(config.ProxyPool{}, nil, &fakeCountryResolver{country: "US"})
	sel, err := m.Resolve(context.Background(), &domain.Account{ID: "acc-1"})
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if !sel.Direct || sel.Proxy != nil {
		t.Fatalf("expected direct selection, got %+v", sel)
	}
}

func TestResolveSingleProxy(t *testing.T) {
	m := NewWithResolver(config.ProxyPool{Proxies: []config.ProxyConfig{
		{ID: "us-1", URL: "http://127.0.0.1:8080", Country: "US"},
	}}, nil, &fakeCountryResolver{country: "US"})
	sel, err := m.Resolve(context.Background(), &domain.Account{ID: "acc-1"})
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if sel.Proxy == nil || sel.Proxy.ID != "us-1" || sel.Proxy.Country != "US" {
		t.Fatalf("unexpected proxy selection: %+v", sel)
	}
}

func TestResolveAccountProxyURLOverride(t *testing.T) {
	m := NewWithResolver(config.ProxyPool{GeoIP: config.GeoIPConfig{Enabled: boolPtr(false)}}, nil, nil)
	sel, err := m.Resolve(context.Background(), &domain.Account{
		ID:    "acc-1",
		Proxy: "http://127.0.0.1:8080",
	})
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if sel.Proxy == nil || sel.Proxy.URL != "http://127.0.0.1:8080" || sel.Proxy.Country != "ZZ" {
		t.Fatalf("unexpected explicit URL selection: %+v", sel)
	}
}

func TestResolveAccountProxyIDOverride(t *testing.T) {
	m := NewWithResolver(config.ProxyPool{Proxies: []config.ProxyConfig{
		{ID: "us-1", URL: "http://127.0.0.1:8080", Country: "US"},
		{ID: "jp-1", URL: "http://127.0.0.2:8080", Country: "JP"},
	}}, nil, nil)
	sel, err := m.Resolve(context.Background(), &domain.Account{ID: "acc-1", Proxy: "jp-1"})
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if sel.Proxy == nil || sel.Proxy.ID != "jp-1" {
		t.Fatalf("expected jp-1 override, got %+v", sel)
	}
}

func TestResolveAccountBindingCountry(t *testing.T) {
	m := NewWithResolver(config.ProxyPool{
		Proxies: []config.ProxyConfig{
			{ID: "us-1", URL: "http://127.0.0.1:8080", Country: "US"},
			{ID: "jp-1", URL: "http://127.0.0.2:8080", Country: "JP"},
		},
		AccountBindings: map[string]string{"acc-1": "country:JP"},
	}, nil, nil)
	sel, err := m.Resolve(context.Background(), &domain.Account{ID: "acc-1"})
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if sel.Proxy == nil || sel.Proxy.Country != "JP" {
		t.Fatalf("expected JP binding, got %+v", sel)
	}
}

func TestResolveStickyCountryWithinWindow(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	m := NewWithResolver(config.ProxyPool{
		StickyWindow: 24 * time.Hour,
		Proxies: []config.ProxyConfig{
			{ID: "us-1", URL: "http://127.0.0.1:8080", Country: "US"},
			{ID: "jp-1", URL: "http://127.0.0.2:8080", Country: "JP"},
		},
	}, nil, nil)
	m.now = func() time.Time { return now }
	first, err := m.Resolve(context.Background(), &domain.Account{ID: "acc-1"})
	if err != nil {
		t.Fatalf("first Resolve error: %v", err)
	}
	second, err := m.Resolve(context.Background(), &domain.Account{ID: "acc-1"})
	if err != nil {
		t.Fatalf("second Resolve error: %v", err)
	}
	if first.Proxy == nil || second.Proxy == nil || first.Proxy.Country != second.Proxy.Country {
		t.Fatalf("expected same country within sticky window, first=%+v second=%+v", first, second)
	}
	other, err := m.Resolve(context.Background(), &domain.Account{ID: "acc-2"})
	if err != nil {
		t.Fatalf("other Resolve error: %v", err)
	}
	if other.Proxy == nil || other.Proxy.Country != "JP" {
		t.Fatalf("expected next account to use balanced JP proxy, got %+v", other)
	}
}

func TestResolveCanChangeCountryAfterWindow(t *testing.T) {
	now := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	m := NewWithResolver(config.ProxyPool{
		StickyWindow: 24 * time.Hour,
		Proxies: []config.ProxyConfig{
			{ID: "us-1", URL: "http://127.0.0.1:8080", Country: "US"},
			{ID: "jp-1", URL: "http://127.0.0.2:8080", Country: "JP"},
		},
	}, nil, nil)
	m.now = func() time.Time { return now }
	first, err := m.Resolve(context.Background(), &domain.Account{ID: "acc-1"})
	if err != nil {
		t.Fatalf("first Resolve error: %v", err)
	}
	now = now.Add(25 * time.Hour)
	second, err := m.Resolve(context.Background(), &domain.Account{ID: "acc-1"})
	if err != nil {
		t.Fatalf("second Resolve error: %v", err)
	}
	if first.Proxy == nil || second.Proxy == nil || first.Proxy.Country == second.Proxy.Country {
		t.Fatalf("expected country can change after sticky window, first=%+v second=%+v", first, second)
	}
}

func TestResolveSameCountryCanRotateProxy(t *testing.T) {
	m := NewWithResolver(config.ProxyPool{
		StickyWindow: 24 * time.Hour,
		Proxies: []config.ProxyConfig{
			{ID: "us-1", URL: "http://127.0.0.1:8080", Country: "US"},
			{ID: "us-2", URL: "http://127.0.0.2:8080", Country: "US"},
			{ID: "jp-1", URL: "http://127.0.0.3:8080", Country: "JP"},
		},
	}, nil, nil)
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		sel, err := m.Resolve(context.Background(), &domain.Account{ID: "acc-1"})
		if err != nil {
			t.Fatalf("Resolve #%d error: %v", i, err)
		}
		if sel.Proxy == nil || sel.Proxy.Country != "US" {
			t.Fatalf("expected sticky US proxy, got %+v", sel)
		}
		seen[sel.Proxy.ID] = true
	}
	if !seen["us-1"] || !seen["us-2"] {
		t.Fatalf("expected same-country rotation across us-1/us-2, seen=%v", seen)
	}
}

func TestResolveMissingCountryUsesResolver(t *testing.T) {
	resolver := &fakeCountryResolver{country: "jp"}
	m := NewWithResolver(config.ProxyPool{Proxies: []config.ProxyConfig{
		{ID: "p-1", URL: "http://127.0.0.1:8080"},
	}}, nil, resolver)
	sel, err := m.Resolve(context.Background(), &domain.Account{ID: "acc-1"})
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if sel.Proxy == nil || sel.Proxy.Country != "JP" || resolver.calls != 1 {
		t.Fatalf("expected resolver-filled JP country, selection=%+v calls=%d", sel, resolver.calls)
	}
}
