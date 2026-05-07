package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// DoHResolver performs DNS lookups over HTTPS to prevent DNS queries from
// being visible to ISPs or firewalls. Uses Cloudflare 1.1.1.1 by default.
type DoHResolver struct {
	url    string
	client *http.Client
	cache  sync.Map // domain → cached result
}

type dohEntry struct {
	ips     []string
	expires time.Time
}

// NewDoHResolver creates a DNS-over-HTTPS resolver.
func NewDoHResolver(url string) *DoHResolver {
	if url == "" {
		url = "https://1.1.1.1/dns-query"
	}
	return &DoHResolver{
		url: url,
		client: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSHandshakeTimeout: 3 * time.Second,
			},
		},
	}
}

// LookupHost resolves a hostname to IP addresses via DoH.
func (r *DoHResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	// Check cache first.
	if v, ok := r.cache.Load(host); ok {
		entry := v.(*dohEntry)
		if time.Now().Before(entry.expires) {
			return entry.ips, nil
		}
		r.cache.Delete(host)
	}

	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s?name=%s&type=A", r.url, url.QueryEscape(host)), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/dns-json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("doh request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var dohResp struct {
		Answer []struct {
			Data string `json:"data"`
			TTL  int    `json:"TTL"`
		} `json:"Answer"`
	}
	if err := json.Unmarshal(body, &dohResp); err != nil {
		return nil, fmt.Errorf("doh parse: %w", err)
	}

	var ips []string
	minTTL := 300
	for _, a := range dohResp.Answer {
		if net.ParseIP(a.Data) != nil {
			ips = append(ips, a.Data)
			if a.TTL < minTTL {
				minTTL = a.TTL
			}
		}
	}
	if len(ips) == 0 {
		return net.DefaultResolver.LookupHost(ctx, host)
	}

	r.cache.Store(host, &dohEntry{
		ips:     ips,
		expires: time.Now().Add(time.Duration(minTTL) * time.Second),
	})
	return ips, nil
}

// DialContext returns a dial function that uses DoH for DNS resolution.
func (r *DoHResolver) DialContext() func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}

		ips, err := r.LookupHost(ctx, host)
		if err != nil {
			return nil, err
		}

		var lastErr error
		for _, ip := range ips {
			conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip, port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}
