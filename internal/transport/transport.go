// Package transport provides shared, tuned HTTP transports for upstream providers.
//
// Anti-detection layers:
//
//	L5a: TLS ClientHello fingerprint (JA3/JA4) via utls — Node.js 24.x
//	L5b: HTTP/2 SETTINGS frame fingerprint — Node.js INITIAL_WINDOW_SIZE, HEADER_TABLE_SIZE
//	L5c: Header ordering — insertion order, not alphabetical
package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// Options configures the shared transport.
type Options struct {
	MaxIdleConnsPerHost int
	Timeout             time.Duration
	TLSCacheSize        int
}

// ForProvider returns a tuned *http.Client with standard Go TLS + Node.js H2 settings.
func ForProvider(host string, opts Options) *http.Client {
	if opts.MaxIdleConnsPerHost <= 0 {
		opts.MaxIdleConnsPerHost = 32
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 600 * time.Second
	}
	if opts.TLSCacheSize <= 0 {
		opts.TLSCacheSize = 128
	}

	tlsCfg := &tls.Config{
		ClientSessionCache: tls.NewLRUClientSessionCache(opts.TLSCacheSize),
		MinVersion:         tls.VersionTLS12,
		NextProtos:         []string{"h2", "http/1.1"},
	}

	transport := &http.Transport{
		DialContext:           doh.DialContext(),
		TLSClientConfig:       tlsCfg,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          opts.MaxIdleConnsPerHost * 8,
		MaxIdleConnsPerHost:   opts.MaxIdleConnsPerHost,
		MaxConnsPerHost:       opts.MaxIdleConnsPerHost * 2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}

	// Override HTTP/2 settings to match Node.js 24.x fingerprint.
	h2Transport, err := http2.ConfigureTransports(transport)
	if err == nil && h2Transport != nil {
		h2Transport.MaxDecoderHeaderTableSize = 65536
		h2Transport.MaxEncoderHeaderTableSize = 65536
		h2Transport.MaxHeaderListSize = 262144
		h2Transport.MaxReadFrameSize = 16384
	}

	return &http.Client{
		Transport: transport,
		Timeout:   opts.Timeout,
	}
}

// ForProviderProxy returns a standard HTTP client routed through proxyURL. The
// shared utls transport cannot reuse http.Transport.Proxy because TLS is dialed
// manually, so proxy egress intentionally uses the standard Go transport.
func ForProviderProxy(proxyRaw string, opts Options) (*http.Client, error) {
	if opts.MaxIdleConnsPerHost <= 0 {
		opts.MaxIdleConnsPerHost = 32
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 600 * time.Second
	}
	if opts.TLSCacheSize <= 0 {
		opts.TLSCacheSize = 128
	}
	proxyURL, err := url.Parse(proxyRaw)
	if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
		return nil, fmt.Errorf("invalid proxy url")
	}
	tlsCfg := &tls.Config{
		ClientSessionCache: tls.NewLRUClientSessionCache(opts.TLSCacheSize),
		MinVersion:         tls.VersionTLS12,
		NextProtos:         []string{"h2", "http/1.1"},
	}
	tr := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig:       tlsCfg,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          opts.MaxIdleConnsPerHost * 8,
		MaxIdleConnsPerHost:   opts.MaxIdleConnsPerHost,
		MaxConnsPerHost:       opts.MaxIdleConnsPerHost * 2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}
	if h2Transport, err := http2.ConfigureTransports(tr); err == nil && h2Transport != nil {
		h2Transport.MaxDecoderHeaderTableSize = 65536
		h2Transport.MaxEncoderHeaderTableSize = 65536
		h2Transport.MaxHeaderListSize = 262144
		h2Transport.MaxReadFrameSize = 16384
	}
	return &http.Client{Transport: tr, Timeout: opts.Timeout}, nil
}

// ForProviderUTLS returns a tuned *http.Client with utls Node.js 24.x fingerprint.
// JA3: 44f88fca027f27bab4bb08d4af15f23e  JA4: t13d1714h1_5b57614c22b0_7baf387fc6ff
func ForProviderUTLS(host string, opts Options) *http.Client {
	if opts.MaxIdleConnsPerHost <= 0 {
		opts.MaxIdleConnsPerHost = 32
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 600 * time.Second
	}

	// utls handles TLS handshake itself (for JA3 fingerprinting).
	// Go's http.Transport + http2.ConfigureTransports cannot detect the
	// negotiated ALPN from utls, so HTTP/2 connections get misrouted to
	// the HTTP/1.x reader → "malformed HTTP response" on h2 frames.
	//
	// Fix: use a dedicated http2.Transport that directly wraps utls conns.
	// We still need an http.Transport for HTTP/1.1 fallback, and we probe
	// ALPN from the utls conn to decide which transport handles the request.
	h1Transport := &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialUTLSH1Only(ctx, network, addr)
		},
		MaxIdleConns:          opts.MaxIdleConnsPerHost * 8,
		MaxIdleConnsPerHost:   opts.MaxIdleConnsPerHost,
		MaxConnsPerHost:       opts.MaxIdleConnsPerHost * 2,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}

	h2Transport := &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return dialUTLS(ctx, network, addr)
		},
		MaxDecoderHeaderTableSize: 65536,
		MaxEncoderHeaderTableSize: 65536,
		MaxHeaderListSize:         262144,
		MaxReadFrameSize:          16384,
		DisableCompression:        true,
	}

	return &http.Client{
		Transport: &alpnSwitchTransport{h1: h1Transport, h2: h2Transport},
		Timeout:   opts.Timeout,
	}
}

// alpnSwitchTransport tries HTTP/2 first (chatgpt.com serves h2), falls back
// to HTTP/1.1 if the h2 transport errors.
type alpnSwitchTransport struct {
	h1 *http.Transport
	h2 *http2.Transport
}

func (t *alpnSwitchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.h2.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	return t.h1.RoundTrip(req)
}

// dialUTLS performs a TLS handshake using utls with Node.js 24.x ClientHello.
func dialUTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	// Use DoH to resolve DNS, then connect to the resolved IP.
	conn, err := doh.DialContext()(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	spec := buildNodeJS24Spec()
	tlsConn := utls.UClient(conn, &utls.Config{ServerName: host}, utls.HelloCustom)
	if err := tlsConn.ApplyPreset(spec); err != nil {
		conn.Close()
		return nil, fmt.Errorf("apply TLS preset: %w", err)
	}
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}
	return tlsConn, nil
}

// dialUTLSH1Only is like dialUTLS but only negotiates HTTP/1.1 (no h2 ALPN).
// Used as fallback when the HTTP/2 transport fails.
func dialUTLSH1Only(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := doh.DialContext()(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	spec := buildNodeJS24Spec()
	// Override ALPN to HTTP/1.1 only
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			alpn.AlpnProtocols = []string{"http/1.1"}
		}
	}
	tlsConn := utls.UClient(conn, &utls.Config{ServerName: host}, utls.HelloCustom)
	if err := tlsConn.ApplyPreset(spec); err != nil {
		conn.Close()
		return nil, fmt.Errorf("apply TLS preset (h1): %w", err)
	}
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("TLS handshake (h1): %w", err)
	}
	return tlsConn, nil
}

// buildNodeJS24Spec constructs the exact Node.js 24.x ClientHello.
func buildNodeJS24Spec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		CipherSuites: []uint16{
			0x1301, 0x1302, 0x1303, // TLS 1.3
			0xc02b, 0xc02f, 0xc02c, 0xc030, // ECDHE + AES-GCM
			0xcca9, 0xcca8, // ECDHE + ChaCha20
			0xc009, 0xc013, 0xc00a, 0xc014, // ECDHE + AES-CBC
			0x009c, 0x009d, // RSA + AES-GCM
			0x002f, 0x0035, // RSA + AES-CBC
		},
		CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.GREASEEncryptedClientHelloExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.X25519, utls.CurveP256, utls.CurveP384,
			}},
			&utls.SupportedPointsExtension{SupportedPoints: []uint8{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"h2", "http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601, 0x0201,
			}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{uint8(utls.PskModeDHE)}},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
		},
		TLSVersMax: utls.VersionTLS13,
		TLSVersMin: utls.VersionTLS10,
	}
}

// doh is the shared DNS-over-HTTPS resolver for all providers.
var doh = NewDoHResolver("")

// Shared singletons — one per provider. All use utls + DoH.
var (
	Claude = ForProviderUTLS("api.anthropic.com", Options{
		MaxIdleConnsPerHost: 64,
		Timeout:             600 * time.Second,
	})

	Codex = ForProviderUTLS("chatgpt.com", Options{
		MaxIdleConnsPerHost: 64,
		Timeout:             600 * time.Second,
	})

	Gemini = ForProviderUTLS("cloudcode-pa.googleapis.com", Options{
		MaxIdleConnsPerHost: 32,
		Timeout:             600 * time.Second,
	})
)
