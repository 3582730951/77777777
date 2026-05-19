package stealth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// IdentityBundle is the single deployment identity exposed to upstream models.
// It is generated once from the VPS process environment and then reused for
// every downstream CLI request handled by the gateway.
type IdentityBundle struct {
	Username                    string            `json:"username"`
	Hostname                    string            `json:"hostname"`
	Platform                    string            `json:"platform"`
	OS                          string            `json:"os"`
	OSVersion                   string            `json:"os_version"`
	Arch                        string            `json:"arch"`
	Shell                       string            `json:"shell"`
	Terminal                    string            `json:"terminal"`
	Runtime                     string            `json:"runtime"`
	RuntimeVersion              string            `json:"runtime_version"`
	AppVersion                  string            `json:"app_version"`
	IPAddress                   string            `json:"ip_address"`
	ASN                         string            `json:"asn"`
	Region                      string            `json:"region"`
	Timezone                    string            `json:"timezone"`
	DNSResolvers                []string          `json:"dns_resolvers"`
	DNSProvider                 string            `json:"dns_provider"`
	ProxyPresent                bool              `json:"proxy_present"`
	ProxyType                   string            `json:"proxy_type"`
	HTTPProxy                   string            `json:"http_proxy"`
	HTTPSProxy                  string            `json:"https_proxy"`
	AllProxy                    string            `json:"all_proxy"`
	NoProxy                     string            `json:"no_proxy"`
	AnthropicBaseURL            string            `json:"anthropic_base_url"`
	GatewayPresent              bool              `json:"gateway_present"`
	GatewayName                 string            `json:"gateway_name"`
	CertStore                   string            `json:"cert_store"`
	ExtraCACertsPresent         bool              `json:"extra_ca_certs_present"`
	MTLSEnabled                 bool              `json:"mtls_enabled"`
	WebFetchPreflightEnabled    bool              `json:"webfetch_preflight_enabled"`
	NonessentialTrafficDisabled bool              `json:"nonessential_traffic_disabled"`
	TelemetryDisabled           bool              `json:"telemetry_disabled"`
	ErrorReportingDisabled      bool              `json:"error_reporting_disabled"`
	MCPProxyMode                string            `json:"mcp_proxy_mode"`
	DirectConnectionPolicy      string            `json:"direct_connection_policy"`
	StatsigStableID             string            `json:"statsig_stable_id"`
	StatsigUserID               string            `json:"statsig_user_id"`
	FeatureGateSeed             string            `json:"feature_gate_seed"`
	FeatureGates                map[string]bool   `json:"feature_gates"`
	ExperimentGroups            map[string]string `json:"experiment_groups"`
	UserID                      string            `json:"user_id"`
	SessionID                   string            `json:"session_id"`
	AccountID                   string            `json:"account_id"`
	AccountUUID                 string            `json:"account_uuid"`
	OrganizationID              string            `json:"organization_id"`
	OrganizationUUID            string            `json:"organization_uuid"`
	Email                       string            `json:"email"`
}

// IdentityAliases are client-side strings that should be folded into the
// deployment identity. Production aliases are collected from the VPS
// environment; tests can supply explicit client aliases.
type IdentityAliases struct {
	Usernames []string
	Hostnames []string
	Shells    []string
	Emails    []string
}

type identityReplacement struct {
	from string
	to   string
}

// IdentityRewriter replaces user and workstation identifiers with the single
// deployment identity. It handles both decoded text and raw JSON request bodies.
type IdentityRewriter struct {
	bundle          IdentityBundle
	textRepls       []identityReplacement
	tokenRepls      []identityReplacement
	userLabelRE     *regexp.Regexp
	hostLabelRE     *regexp.Regexp
	platformLabelRE *regexp.Regexp
	osLabelRE       *regexp.Regexp
	archLabelRE     *regexp.Regexp
	termLabelRE     *regexp.Regexp
	appLabelRE      *regexp.Regexp
	netLabelRE      *regexp.Regexp
	proxyLabelRE    *regexp.Regexp
	statsigLabelRE  *regexp.Regexp
	emailLabelRE    *regexp.Regexp
	sidLabelRE      *regexp.Regexp
}

type persistedIdentity struct {
	Version           int            `json:"version"`
	CreatedAt         string         `json:"created_at"`
	Provider          string         `json:"provider,omitempty"`
	UpstreamAccountID string         `json:"upstream_account_id,omitempty"`
	Bundle            IdentityBundle `json:"bundle"`
}

type IdentityStore struct {
	path string
}

// NewDeploymentIdentityRewriter builds a process-wide identity rewriter from
// the VPS environment.
func NewDeploymentIdentityRewriter() *IdentityRewriter {
	bundle := DeploymentIdentityBundle()
	return NewIdentityRewriter(bundle, DeploymentIdentityAliases(bundle))
}

// NewPersistentDeploymentIdentityRewriter loads the deployment identity from
// path. If the file does not exist it generates exactly one bundle from the VPS
// environment and persists it before returning. Existing files are never
// regenerated from the current process environment.
func NewPersistentDeploymentIdentityRewriter(path string) (*IdentityRewriter, error) {
	bundle, err := LoadOrCreateIdentityBundle(path)
	if err != nil {
		return nil, err
	}
	return NewIdentityRewriter(bundle, DeploymentIdentityAliases(bundle)), nil
}

func NewIdentityStore(path string) *IdentityStore {
	if strings.TrimSpace(path) == "" {
		path = "data/identity_bundle.json"
	}
	return &IdentityStore{path: path}
}

func (s *IdentityStore) RewriterForAccount(accountID, provider, accountEmail string) (*IdentityRewriter, error) {
	if s == nil {
		return nil, nil
	}
	bundle, err := LoadOrCreateAccountIdentityBundle(s.path, provider, accountID, accountEmail)
	if err != nil {
		return nil, err
	}
	return NewIdentityRewriter(bundle, DeploymentIdentityAliases(bundle)), nil
}

// LoadOrCreateIdentityBundle loads the persisted identity bundle, creating it
// once when absent. The resulting bundle is stable across project restarts.
func LoadOrCreateIdentityBundle(path string) (IdentityBundle, error) {
	if strings.TrimSpace(path) == "" {
		path = "data/identity_bundle.json"
	}
	if stored, err := readPersistedIdentity(path); err == nil {
		bundle := normalizeIdentityBundle(stored.Bundle)
		if !jsonValuesEqual(stored.Bundle, bundle) {
			stored.Bundle = bundle
			if stored.Version < 3 {
				stored.Version = 3
			}
			if stored.CreatedAt == "" {
				stored.CreatedAt = time.Now().UTC().Format(time.RFC3339)
			}
			if err := writePersistedIdentity(path, stored); err != nil {
				return IdentityBundle{}, err
			}
		}
		return bundle, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return IdentityBundle{}, err
	}

	bundle := normalizeIdentityBundle(DeploymentIdentityBundle())
	if err := writeIdentityBundleOnce(path, bundle); err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, readErr := readIdentityBundle(path)
			if readErr != nil {
				return IdentityBundle{}, readErr
			}
			return normalizeIdentityBundle(existing), nil
		}
		return IdentityBundle{}, err
	}
	return bundle, nil
}

func LoadOrCreateAccountIdentityBundle(path, provider, accountID, accountEmail string) (IdentityBundle, error) {
	if strings.TrimSpace(path) == "" {
		path = "data/identity_bundle.json"
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "unknown"
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		accountID = "default"
	}
	accountPath := accountIdentityPath(path, provider, accountID)
	if stored, err := readPersistedIdentity(accountPath); err == nil {
		bundle := normalizeIdentityBundle(stored.Bundle)
		if !jsonValuesEqual(stored.Bundle, bundle) {
			stored.Bundle = bundle
			if stored.Version < 3 {
				stored.Version = 3
			}
			if stored.Provider == "" {
				stored.Provider = provider
			}
			if stored.UpstreamAccountID == "" {
				stored.UpstreamAccountID = accountID
			}
			if stored.CreatedAt == "" {
				stored.CreatedAt = time.Now().UTC().Format(time.RFC3339)
			}
			if err := writePersistedIdentity(accountPath, stored); err != nil {
				return IdentityBundle{}, err
			}
		}
		return bundle, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return IdentityBundle{}, err
	}

	base, err := LoadOrCreateIdentityBundle(path)
	if err != nil {
		return IdentityBundle{}, err
	}
	bundle := deriveAccountIdentityBundle(base, provider, accountID, accountEmail)
	if err := writeAccountIdentityBundleOnce(accountPath, provider, accountID, bundle); err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, readErr := readIdentityBundle(accountPath)
			if readErr != nil {
				return IdentityBundle{}, readErr
			}
			return normalizeIdentityBundle(existing), nil
		}
		return IdentityBundle{}, err
	}
	return bundle, nil
}

func readIdentityBundle(path string) (IdentityBundle, error) {
	stored, err := readPersistedIdentity(path)
	if err != nil {
		return IdentityBundle{}, err
	}
	return stored.Bundle, nil
}

func readPersistedIdentity(path string) (persistedIdentity, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return persistedIdentity{}, err
	}
	var wrapped persistedIdentity
	if err := json.Unmarshal(b, &wrapped); err == nil && wrapped.Bundle.Username != "" {
		if wrapped.CreatedAt == "" {
			wrapped.CreatedAt = time.Now().UTC().Format(time.RFC3339)
		}
		return wrapped, nil
	}
	var direct IdentityBundle
	if err := json.Unmarshal(b, &direct); err != nil {
		return persistedIdentity{}, fmt.Errorf("parse identity bundle %s: %w", path, err)
	}
	if direct.Username == "" {
		return persistedIdentity{}, fmt.Errorf("parse identity bundle %s: missing username", path)
	}
	return persistedIdentity{
		Version:   1,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Bundle:    direct,
	}, nil
}

func writeIdentityBundleOnce(path string, bundle IdentityBundle) error {
	return writePersistedIdentityOnce(path, persistedIdentity{
		Version:   1,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Bundle:    bundle,
	})
}

func writeAccountIdentityBundleOnce(path, provider, accountID string, bundle IdentityBundle) error {
	return writePersistedIdentityOnce(path, persistedIdentity{
		Version:           2,
		CreatedAt:         time.Now().UTC().Format(time.RFC3339),
		Provider:          provider,
		UpstreamAccountID: accountID,
		Bundle:            bundle,
	})
}

func writePersistedIdentityOnce(path string, payload persistedIdentity) error {
	return writePersistedIdentityFile(path, payload, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
}

func writePersistedIdentity(path string, payload persistedIdentity) error {
	return writePersistedIdentityFile(path, payload, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
}

func writePersistedIdentityFile(path string, payload persistedIdentity, flags int) error {
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func accountIdentityPath(path, provider, accountID string) string {
	dir := path + ".accounts"
	name := cleanToken(provider+"_"+accountID, '_')
	if name == "" {
		name = "default"
	}
	return filepath.Join(dir, name+".json")
}

func deriveAccountIdentityBundle(base IdentityBundle, provider, accountID, accountEmail string) IdentityBundle {
	accountSeed := strings.Join([]string{
		base.Hostname, base.Username, base.OS, base.Arch,
		base.IPAddress, base.ASN, base.Region, base.Timezone,
		strings.Join(base.DNSResolvers, ","),
		strconv.FormatBool(base.ProxyPresent), base.ProxyType,
		base.HTTPProxy, base.HTTPSProxy, base.AllProxy, base.NoProxy,
		base.AnthropicBaseURL, strconv.FormatBool(base.GatewayPresent),
		base.GatewayName, base.CertStore,
		strconv.FormatBool(base.ExtraCACertsPresent),
		strconv.FormatBool(base.MTLSEnabled),
		strconv.FormatBool(base.WebFetchPreflightEnabled),
		strconv.FormatBool(base.NonessentialTrafficDisabled),
		strconv.FormatBool(base.TelemetryDisabled),
		strconv.FormatBool(base.ErrorReportingDisabled),
		base.MCPProxyMode, base.DirectConnectionPolicy,
		provider, accountID, strings.ToLower(strings.TrimSpace(accountEmail)),
	}, "\x00")
	h := shortHash(accountSeed, 64)
	suffix := h[:8]
	bundle := base
	bundle.Username = cleanUserToken(base.Username + "_" + suffix[:6])
	bundle.Hostname = cleanHostToken(base.Hostname + "-" + suffix[:6])
	bundle.UserID = "user_" + h[:24]
	bundle.SessionID = "sess_" + h[24:48]
	bundle.AccountID = "acct_" + h[40:64]
	bundle.AccountUUID = h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
	bundle.OrganizationID = "org_" + h[32:56]
	bundle.OrganizationUUID = h[32:40] + "-" + h[40:44] + "-" + h[44:48] + "-" + h[48:52] + "-" + h[52:64]
	bundle.Email = "claude-" + suffix + "@" + base.Hostname + ".local"
	bundle.StatsigStableID = "stable_" + h[:32]
	bundle.StatsigUserID = "statsig_user_" + h[32:56]
	bundle.FeatureGateSeed = h[8:40]
	bundle.FeatureGates = deterministicFeatureGates(h)
	bundle.ExperimentGroups = deterministicExperimentGroups(h)
	return normalizeIdentityBundle(bundle)
}

// NewIdentityRewriter builds a rewriter for bundle and aliases. Empty aliases
// are ignored. Longer aliases are applied first.
func NewIdentityRewriter(bundle IdentityBundle, aliases IdentityAliases) *IdentityRewriter {
	bundle = normalizeIdentityBundle(bundle)
	r := &IdentityRewriter{
		bundle:          bundle,
		userLabelRE:     regexp.MustCompile(`(?i)\b(user(?:name)?|user\.name|logname|login|USER|LOGNAME|USERNAME)\b(\s*[:=]\s*)([A-Za-z0-9._@-]+)`),
		hostLabelRE:     regexp.MustCompile(`(?i)\b(hostname|host\.name|machine(?: name)?|computer(?: name)?)\b(\s*[:=]\s*)([A-Za-z0-9][A-Za-z0-9._-]{0,127})`),
		platformLabelRE: regexp.MustCompile(`(?i)\b(platform|os\.platform)\b(\s*[:=]\s*)([A-Za-z0-9._-]+)`),
		osLabelRE:       regexp.MustCompile(`(?i)\b(os\.type|os\.version|operating system)\b(\s*[:=]\s*)([A-Za-z0-9._-]+)`),
		archLabelRE:     regexp.MustCompile(`(?i)\b(host\.arch|architecture|arch)\b(\s*[:=]\s*)([A-Za-z0-9._-]+)`),
		termLabelRE:     regexp.MustCompile(`(?i)\b(terminal\.type|terminal_type|terminal)\b(\s*[:=]\s*)([A-Za-z0-9._@/-]+)`),
		appLabelRE:      regexp.MustCompile(`(?i)\b(app\.version|app_version|claude_code\.version|runtime\.version|runtime_version|runtime)\b(\s*[:=]\s*)([A-Za-z0-9._@/-]+)`),
		netLabelRE:      regexp.MustCompile(`(?i)\b(ip|ip_address|client_ip|asn|region|timezone|time_zone|dns_provider|dns\.provider|dns_resolvers|dns\.resolvers)\b(\s*[:=]\s*)([A-Za-z0-9._:@,\[\]-]+)`),
		proxyLabelRE:    regexp.MustCompile(`(?i)\b(proxy_present|proxy\.present|proxy_type|proxy\.type|http_proxy|https_proxy|all_proxy|no_proxy|anthropic_base_url|gateway_present|gateway\.present|gateway_name|gateway\.name|cert_store|claude_code_cert_store|extra_ca_certs_present|extra_ca_certs\.present|mtls_enabled|mtls\.enabled|webfetch_preflight_enabled|webfetch\.preflight_enabled|skip_webfetch_preflight|nonessential_traffic_disabled|telemetry_disabled|error_reporting_disabled|mcp_proxy_mode|direct_connection_policy)\b(\s*[:=]\s*)([^\s;]+)`),
		statsigLabelRE:  regexp.MustCompile(`(?i)\b(statsig_stable_id|statsig\.stable_id|stable_id|statsig_user_id|statsig\.user_id|feature_gate_seed|feature\.gate\.seed)\b(\s*[:=]\s*)([A-Za-z0-9._@-]+)`),
		emailLabelRE:    regexp.MustCompile(`(?i)\b(user\.email|user_email|email)\b(\s*[:=]\s*)([A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+)`),
		sidLabelRE:      regexp.MustCompile(`(?i)\b(session\.id|session_id|user\.id|user_id|user\.account_uuid|account_uuid|account\.id|account_id|organization\.id|organization_id|organization_uuid|org_id|org_uuid)\b(\s*[:=]\s*)([A-Za-z0-9._@-]+)`),
	}
	for _, email := range uniqueStrings(aliases.Emails) {
		r.addTokenReplacement(email, bundle.Email)
	}

	sortReplacements(r.textRepls)
	sortReplacements(r.tokenRepls)
	return r
}

func (r *IdentityRewriter) Bundle() IdentityBundle {
	if r == nil {
		return IdentityBundle{}
	}
	return r.bundle
}

func (r *IdentityRewriter) addTextReplacement(from, to string) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" || from == to {
		return
	}
	r.textRepls = append(r.textRepls, identityReplacement{from: from, to: to})
}

func (r *IdentityRewriter) addTokenReplacement(from, to string) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" || from == to {
		return
	}
	r.tokenRepls = append(r.tokenRepls, identityReplacement{from: from, to: to})
}

// DeploymentIdentityBundle returns the canonical identity generated from the
// current VPS process environment.
func DeploymentIdentityBundle() IdentityBundle {
	hostname, _ := os.Hostname()
	hostname = cleanHostToken(firstNonEmpty(hostname, os.Getenv("HOSTNAME")))

	username := ""
	if cur, err := user.Current(); err == nil && cur != nil {
		username = cur.Username
	}
	username = cleanUserToken(firstNonEmpty(os.Getenv("USER"), os.Getenv("LOGNAME"), os.Getenv("USERNAME"), username))
	if username == "" {
		username = "root"
	}
	if hostname == "" {
		hostname = "vps-" + shortHash(username, 8)
	}

	shell := firstNonEmpty(os.Getenv("SHELL"), defaultShell())
	term := firstNonEmpty(os.Getenv("TERM_PROGRAM"), os.Getenv("TERM"), "xterm-256color")
	primaryIP := detectPrimaryIP()
	dnsResolvers := detectDNSResolvers()
	timezone := detectTimezone()
	region := detectRegion(timezone, "")
	dnsProvider := detectDNSProvider(dnsResolvers)
	proxy := detectProxyFingerprint()

	seed := strings.Join([]string{
		hostname, username, runtime.GOOS, runtime.GOARCH, shell,
		primaryIP, strings.Join(dnsResolvers, ","), timezone,
		proxy.ProxyType, proxy.HTTPProxy, proxy.HTTPSProxy, proxy.AllProxy,
		proxy.NoProxy, proxy.AnthropicBaseURL, proxy.GatewayName,
		proxy.CertStore, proxy.MCPProxyMode, proxy.DirectConnectionPolicy,
	}, "\x00")
	sum := sha256.Sum256([]byte(seed))
	h := hex.EncodeToString(sum[:])
	if region == "" {
		region = detectRegion(timezone, h)
	}

	return IdentityBundle{
		Username:                    username,
		Hostname:                    hostname,
		Platform:                    runtime.GOOS,
		OS:                          runtime.GOOS,
		OSVersion:                   detectOSVersion(),
		Arch:                        runtime.GOARCH,
		Shell:                       filepath.ToSlash(shell),
		Terminal:                    term,
		Runtime:                     "node",
		RuntimeVersion:              "v24.3.0",
		AppVersion:                  "2.1.138",
		IPAddress:                   primaryIP,
		ASN:                         deterministicASN(h),
		Region:                      region,
		Timezone:                    timezone,
		DNSResolvers:                dnsResolvers,
		DNSProvider:                 dnsProvider,
		ProxyPresent:                proxy.ProxyPresent,
		ProxyType:                   proxy.ProxyType,
		HTTPProxy:                   proxy.HTTPProxy,
		HTTPSProxy:                  proxy.HTTPSProxy,
		AllProxy:                    proxy.AllProxy,
		NoProxy:                     proxy.NoProxy,
		AnthropicBaseURL:            proxy.AnthropicBaseURL,
		GatewayPresent:              proxy.GatewayPresent,
		GatewayName:                 proxy.GatewayName,
		CertStore:                   proxy.CertStore,
		ExtraCACertsPresent:         proxy.ExtraCACertsPresent,
		MTLSEnabled:                 proxy.MTLSEnabled,
		WebFetchPreflightEnabled:    proxy.WebFetchPreflightEnabled,
		NonessentialTrafficDisabled: proxy.NonessentialTrafficDisabled,
		TelemetryDisabled:           proxy.TelemetryDisabled,
		ErrorReportingDisabled:      proxy.ErrorReportingDisabled,
		MCPProxyMode:                proxy.MCPProxyMode,
		DirectConnectionPolicy:      proxy.DirectConnectionPolicy,
		StatsigStableID:             "stable_" + h[:32],
		StatsigUserID:               "statsig_user_" + h[32:56],
		FeatureGateSeed:             h[8:40],
		FeatureGates:                deterministicFeatureGates(h),
		ExperimentGroups:            deterministicExperimentGroups(h),
		UserID:                      "user_" + h[:24],
		SessionID:                   "sess_" + h[24:48],
		AccountID:                   "acct_" + h[48:64],
		AccountUUID:                 h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32],
		OrganizationID:              "org_" + h[32:56],
		OrganizationUUID:            h[32:40] + "-" + h[40:44] + "-" + h[44:48] + "-" + h[48:52] + "-" + h[52:64],
		Email:                       username + "@" + hostname + ".local",
	}
}

// DeploymentIdentityAliases collects known local aliases from the same VPS
// environment. In production these are usually no-ops, but they make the
// generated bundle authoritative for labels emitted by local CLIs.
func DeploymentIdentityAliases(bundle IdentityBundle) IdentityAliases {
	var curUsername string
	if cur, err := user.Current(); err == nil && cur != nil {
		curUsername = cur.Username
	}
	hostname, _ := os.Hostname()
	return IdentityAliases{
		Usernames: []string{
			os.Getenv("USER"), os.Getenv("LOGNAME"), os.Getenv("USERNAME"),
			curUsername, cleanUserToken(curUsername),
		},
		Hostnames: []string{
			os.Getenv("HOSTNAME"), hostname, bundle.Hostname,
		},
		Shells: []string{
			os.Getenv("SHELL"), bundle.Shell,
		},
		Emails: []string{
			os.Getenv("GIT_AUTHOR_EMAIL"), os.Getenv("GIT_COMMITTER_EMAIL"), bundle.Email,
		},
	}
}

func normalizeIdentityBundle(bundle IdentityBundle) IdentityBundle {
	bundle.Username = cleanUserToken(bundle.Username)
	if bundle.Username == "" {
		bundle.Username = "root"
	}
	bundle.Hostname = cleanHostToken(bundle.Hostname)
	if bundle.Hostname == "" {
		bundle.Hostname = "vps-" + shortHash(bundle.Username, 8)
	}
	if bundle.Platform == "" {
		bundle.Platform = runtime.GOOS
	}
	if bundle.OS == "" {
		bundle.OS = runtime.GOOS
	}
	if bundle.OSVersion == "" {
		bundle.OSVersion = detectOSVersion()
	}
	if bundle.Arch == "" {
		bundle.Arch = runtime.GOARCH
	}
	if bundle.Shell == "" {
		bundle.Shell = defaultShell()
	}
	if bundle.Terminal == "" {
		bundle.Terminal = "xterm-256color"
	}
	if bundle.Runtime == "" {
		bundle.Runtime = "node"
	}
	if bundle.RuntimeVersion == "" {
		bundle.RuntimeVersion = "v24.3.0"
	}
	if bundle.AppVersion == "" {
		bundle.AppVersion = "2.1.138"
	}
	if bundle.IPAddress == "" {
		bundle.IPAddress = detectPrimaryIP()
	}
	if bundle.Timezone == "" {
		bundle.Timezone = detectTimezone()
	}
	bundle.DNSResolvers = normalizeDNSResolvers(bundle.DNSResolvers)
	if len(bundle.DNSResolvers) == 0 {
		bundle.DNSResolvers = detectDNSResolvers()
	}
	if bundle.DNSProvider == "" {
		bundle.DNSProvider = detectDNSProvider(bundle.DNSResolvers)
	}
	proxy := normalizeProxyFields(bundle)
	bundle.ProxyPresent = proxy.ProxyPresent
	bundle.ProxyType = proxy.ProxyType
	bundle.HTTPProxy = proxy.HTTPProxy
	bundle.HTTPSProxy = proxy.HTTPSProxy
	bundle.AllProxy = proxy.AllProxy
	bundle.NoProxy = proxy.NoProxy
	bundle.AnthropicBaseURL = proxy.AnthropicBaseURL
	bundle.GatewayPresent = proxy.GatewayPresent
	bundle.GatewayName = proxy.GatewayName
	bundle.CertStore = proxy.CertStore
	bundle.ExtraCACertsPresent = proxy.ExtraCACertsPresent
	bundle.MTLSEnabled = proxy.MTLSEnabled
	bundle.WebFetchPreflightEnabled = proxy.WebFetchPreflightEnabled
	bundle.NonessentialTrafficDisabled = proxy.NonessentialTrafficDisabled
	bundle.TelemetryDisabled = proxy.TelemetryDisabled
	bundle.ErrorReportingDisabled = proxy.ErrorReportingDisabled
	bundle.MCPProxyMode = proxy.MCPProxyMode
	bundle.DirectConnectionPolicy = proxy.DirectConnectionPolicy
	seed := strings.Join([]string{
		bundle.Username, bundle.Hostname, bundle.Platform, bundle.OS, bundle.Arch, bundle.Shell,
		bundle.IPAddress, bundle.ASN, bundle.Region, bundle.Timezone,
		strings.Join(bundle.DNSResolvers, ","),
		strconv.FormatBool(bundle.ProxyPresent), bundle.ProxyType,
		bundle.HTTPProxy, bundle.HTTPSProxy, bundle.AllProxy, bundle.NoProxy,
		bundle.AnthropicBaseURL, strconv.FormatBool(bundle.GatewayPresent),
		bundle.GatewayName, bundle.CertStore,
		strconv.FormatBool(bundle.ExtraCACertsPresent),
		strconv.FormatBool(bundle.MTLSEnabled),
		strconv.FormatBool(bundle.WebFetchPreflightEnabled),
		strconv.FormatBool(bundle.NonessentialTrafficDisabled),
		strconv.FormatBool(bundle.TelemetryDisabled),
		strconv.FormatBool(bundle.ErrorReportingDisabled),
		bundle.MCPProxyMode, bundle.DirectConnectionPolicy,
	}, "\x00")
	h := shortHash(seed, 64)
	if bundle.ASN == "" {
		bundle.ASN = deterministicASN(h)
	}
	if bundle.Region == "" {
		bundle.Region = detectRegion(bundle.Timezone, h)
	}
	if bundle.StatsigStableID == "" {
		bundle.StatsigStableID = "stable_" + h[:32]
	}
	if bundle.StatsigUserID == "" {
		bundle.StatsigUserID = "statsig_user_" + h[32:56]
	}
	if bundle.FeatureGateSeed == "" {
		bundle.FeatureGateSeed = h[8:40]
	}
	if len(bundle.FeatureGates) == 0 {
		bundle.FeatureGates = deterministicFeatureGates(h)
	}
	if len(bundle.ExperimentGroups) == 0 {
		bundle.ExperimentGroups = deterministicExperimentGroups(h)
	}
	if bundle.UserID == "" {
		bundle.UserID = "user_" + h[:24]
	}
	if bundle.SessionID == "" {
		bundle.SessionID = "sess_" + h[24:48]
	}
	if bundle.AccountID == "" {
		bundle.AccountID = "acct_" + h[48:64]
	}
	if bundle.AccountUUID == "" {
		bundle.AccountUUID = h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
	}
	if bundle.OrganizationID == "" {
		bundle.OrganizationID = "org_" + h[32:56]
	}
	if bundle.OrganizationUUID == "" {
		bundle.OrganizationUUID = h[32:40] + "-" + h[40:44] + "-" + h[44:48] + "-" + h[48:52] + "-" + h[52:64]
	}
	if bundle.Email == "" {
		bundle.Email = bundle.Username + "@" + bundle.Hostname + ".local"
	}
	bundle.Shell = filepath.ToSlash(bundle.Shell)
	return bundle
}

// ReplaceString rewrites known Claude Code user/account/session and environment
// fingerprint labels to the deployment identity. Project paths and workspace
// locations are intentionally preserved so tool context remains accurate.
func (r *IdentityRewriter) ReplaceString(s string) string {
	if r == nil || s == "" {
		return s
	}
	out := s
	for _, repl := range r.textRepls {
		out = strings.ReplaceAll(out, repl.from, repl.to)
	}
	out = r.rewriteLabeledFields(out)
	for _, repl := range r.tokenRepls {
		out = replaceToken(out, repl.from, repl.to)
	}
	return out
}

func replaceLabelValue(re *regexp.Regexp, s, value string) string {
	return re.ReplaceAllStringFunc(s, func(match string) string {
		parts := re.FindStringSubmatch(match)
		if len(parts) < 4 {
			return match
		}
		return parts[1] + parts[2] + value
	})
}

func (r *IdentityRewriter) rewriteLabeledFields(s string) string {
	out := replaceLabelValue(r.userLabelRE, s, r.bundle.Username)
	out = replaceLabelValue(r.hostLabelRE, out, r.bundle.Hostname)
	out = replaceLabelValue(r.platformLabelRE, out, r.bundle.Platform)
	out = r.osLabelRE.ReplaceAllStringFunc(out, func(match string) string {
		parts := r.osLabelRE.FindStringSubmatch(match)
		if len(parts) < 4 {
			return match
		}
		value := r.bundle.OS
		if strings.EqualFold(parts[1], "os.version") && r.bundle.OSVersion != "" {
			value = r.bundle.OSVersion
		}
		return parts[1] + parts[2] + value
	})
	out = replaceLabelValue(r.archLabelRE, out, r.bundle.Arch)
	out = replaceLabelValue(r.termLabelRE, out, r.bundle.Terminal)
	out = r.appLabelRE.ReplaceAllStringFunc(out, func(match string) string {
		parts := r.appLabelRE.FindStringSubmatch(match)
		if len(parts) < 4 {
			return match
		}
		key := strings.ToLower(parts[1])
		value := r.bundle.Runtime
		if strings.Contains(key, "version") {
			value = r.bundle.RuntimeVersion
		}
		if strings.HasPrefix(key, "app") || strings.HasPrefix(key, "claude_code") {
			value = r.bundle.AppVersion
		}
		return parts[1] + parts[2] + value
	})
	out = r.netLabelRE.ReplaceAllStringFunc(out, func(match string) string {
		parts := r.netLabelRE.FindStringSubmatch(match)
		if len(parts) < 4 {
			return match
		}
		key := normalizeIdentityKey(parts[1])
		value := r.bundle.IPAddress
		switch key {
		case "asn":
			value = r.bundle.ASN
		case "region":
			value = r.bundle.Region
		case "timezone", "time_zone":
			value = r.bundle.Timezone
		case "dns_provider", "dns.provider":
			value = r.bundle.DNSProvider
		case "dns_resolvers", "dns.resolvers":
			value = strings.Join(r.bundle.DNSResolvers, ",")
		}
		return parts[1] + parts[2] + value
	})
	out = r.proxyLabelRE.ReplaceAllStringFunc(out, func(match string) string {
		parts := r.proxyLabelRE.FindStringSubmatch(match)
		if len(parts) < 4 {
			return match
		}
		value := r.proxyLabelValue(normalizeIdentityKey(parts[1]))
		return parts[1] + parts[2] + value
	})
	out = r.statsigLabelRE.ReplaceAllStringFunc(out, func(match string) string {
		parts := r.statsigLabelRE.FindStringSubmatch(match)
		if len(parts) < 4 {
			return match
		}
		key := normalizeIdentityKey(parts[1])
		value := r.bundle.StatsigStableID
		switch key {
		case "statsig_user_id", "statsig.user_id":
			value = r.bundle.StatsigUserID
		case "feature_gate_seed", "feature.gate.seed":
			value = r.bundle.FeatureGateSeed
		}
		return parts[1] + parts[2] + value
	})
	out = replaceLabelValue(r.emailLabelRE, out, r.bundle.Email)
	out = r.sidLabelRE.ReplaceAllStringFunc(out, func(match string) string {
		parts := r.sidLabelRE.FindStringSubmatch(match)
		if len(parts) < 4 {
			return match
		}
		key := strings.ToLower(parts[1])
		value := r.bundle.SessionID
		if strings.HasPrefix(key, "user") {
			value = r.bundle.UserID
		}
		if strings.HasPrefix(key, "account") {
			value = r.bundle.AccountID
		}
		if key == "user.account_uuid" || key == "account_uuid" {
			value = r.bundle.AccountUUID
		}
		if strings.HasPrefix(key, "organization") || strings.HasPrefix(key, "org_") {
			value = r.bundle.OrganizationID
			if strings.Contains(key, "uuid") {
				value = r.bundle.OrganizationUUID
			}
		}
		return parts[1] + parts[2] + value
	})
	return out
}

func (r *IdentityRewriter) proxyLabelValue(key string) string {
	switch key {
	case "proxy_present", "proxy.present":
		return strconv.FormatBool(r.bundle.ProxyPresent)
	case "proxy_type", "proxy.type":
		return r.bundle.ProxyType
	case "http_proxy":
		return r.bundle.HTTPProxy
	case "https_proxy":
		return r.bundle.HTTPSProxy
	case "all_proxy":
		return r.bundle.AllProxy
	case "no_proxy":
		return r.bundle.NoProxy
	case "anthropic_base_url":
		return r.bundle.AnthropicBaseURL
	case "gateway_present", "gateway.present":
		return strconv.FormatBool(r.bundle.GatewayPresent)
	case "gateway_name", "gateway.name":
		return r.bundle.GatewayName
	case "cert_store", "claude_code_cert_store":
		return r.bundle.CertStore
	case "extra_ca_certs_present", "extra_ca_certs.present":
		return strconv.FormatBool(r.bundle.ExtraCACertsPresent)
	case "mtls_enabled", "mtls.enabled":
		return strconv.FormatBool(r.bundle.MTLSEnabled)
	case "webfetch_preflight_enabled", "webfetch.preflight_enabled":
		return strconv.FormatBool(r.bundle.WebFetchPreflightEnabled)
	case "skip_webfetch_preflight":
		return strconv.FormatBool(!r.bundle.WebFetchPreflightEnabled)
	case "nonessential_traffic_disabled":
		return strconv.FormatBool(r.bundle.NonessentialTrafficDisabled)
	case "telemetry_disabled":
		return strconv.FormatBool(r.bundle.TelemetryDisabled)
	case "error_reporting_disabled":
		return strconv.FormatBool(r.bundle.ErrorReportingDisabled)
	case "mcp_proxy_mode":
		return r.bundle.MCPProxyMode
	case "direct_connection_policy":
		return r.bundle.DirectConnectionPolicy
	default:
		return ""
	}
}

// RewriteJSONBody rewrites all JSON string values and preserves the original
// body when no values change. Object keys are intentionally left untouched.
func (r *IdentityRewriter) RewriteJSONBody(body []byte) []byte {
	if r == nil || len(body) == 0 {
		return body
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		next := r.ReplaceString(string(body))
		if next == string(body) {
			return body
		}
		return []byte(next)
	}
	next, changed := r.rewriteJSONValue("", v)
	if !changed {
		return body
	}
	out, err := json.Marshal(next)
	if err != nil {
		return body
	}
	return out
}

func (r *IdentityRewriter) rewriteJSONValue(key string, v any) (any, bool) {
	if forced, ok := r.canonicalJSONValueForKey(key, v); ok {
		return forced, !jsonValuesEqual(v, forced)
	}
	switch t := v.(type) {
	case string:
		next := r.ReplaceString(t)
		return next, next != t
	case []any:
		changed := false
		for i := range t {
			next, ok := r.rewriteJSONValue(key, t[i])
			if ok {
				t[i] = next
				changed = true
			}
		}
		return t, changed
	case map[string]any:
		changed := false
		for k, val := range t {
			next, ok := r.rewriteJSONValue(joinIdentityKey(key, k), val)
			if ok {
				t[k] = next
				changed = true
			}
		}
		return t, changed
	default:
		return v, false
	}
}

func (r *IdentityRewriter) canonicalJSONValueForKey(key string, v any) (any, bool) {
	k := normalizeIdentityKey(key)
	if k == "" {
		return nil, false
	}
	switch k {
	case "username", "user_name", "user.name", "login", "logname":
		return r.bundle.Username, isStringLike(v)
	case "session_id", "session.id":
		return r.bundle.SessionID, isStringLike(v)
	case "user_id", "user.id":
		return r.bundle.UserID, isStringLike(v)
	case "user_account_uuid", "user.account_uuid", "account_uuid":
		return r.bundle.AccountUUID, isStringLike(v)
	case "account_id", "account.id":
		return r.bundle.AccountID, isStringLike(v)
	case "organization_id", "organization.id", "org_id":
		return r.bundle.OrganizationID, isStringLike(v)
	case "organization_uuid", "org_uuid":
		return r.bundle.OrganizationUUID, isStringLike(v)
	case "user_email", "user.email", "email":
		return r.bundle.Email, isStringLike(v)
	case "hostname", "host_name", "host.name", "machine_name", "computer_name":
		return r.bundle.Hostname, isStringLike(v)
	case "platform", "os.platform":
		return r.bundle.Platform, isStringLike(v)
	case "os", "os_type", "os.type", "operating_system":
		return r.bundle.OS, isStringLike(v)
	case "os_version", "os.version":
		return r.bundle.OSVersion, isStringLike(v)
	case "host_arch", "host.arch", "architecture", "arch":
		return r.bundle.Arch, isStringLike(v)
	case "terminal", "terminal_type", "terminal.type":
		return r.bundle.Terminal, isStringLike(v)
	case "runtime", "runtime_name":
		return r.bundle.Runtime, isStringLike(v)
	case "runtime_version", "runtime.version":
		return r.bundle.RuntimeVersion, isStringLike(v)
	case "app_version", "app.version", "claude_code_version", "claude_code.version":
		return r.bundle.AppVersion, isStringLike(v)
	case "ip", "ip_address", "client_ip":
		return r.bundle.IPAddress, isStringLike(v)
	case "asn":
		return r.bundle.ASN, isStringLike(v)
	case "region":
		return r.bundle.Region, isStringLike(v)
	case "timezone", "time_zone":
		return r.bundle.Timezone, isStringLike(v)
	case "dns_provider", "dns.provider":
		return r.bundle.DNSProvider, isStringLike(v)
	case "dns_resolvers", "dns.resolvers":
		if _, ok := v.(string); ok {
			return strings.Join(r.bundle.DNSResolvers, ","), true
		}
		return stringSliceToAny(r.bundle.DNSResolvers), isArrayLike(v)
	case "proxy_present", "proxy.present":
		return canonicalBoolValue(v, r.bundle.ProxyPresent)
	case "proxy_type", "proxy.type":
		return r.bundle.ProxyType, isStringLike(v)
	case "http_proxy":
		return r.bundle.HTTPProxy, isStringLike(v)
	case "https_proxy":
		return r.bundle.HTTPSProxy, isStringLike(v)
	case "all_proxy":
		return r.bundle.AllProxy, isStringLike(v)
	case "no_proxy":
		return r.bundle.NoProxy, isStringLike(v)
	case "anthropic_base_url":
		return r.bundle.AnthropicBaseURL, isStringLike(v)
	case "gateway_present", "gateway.present":
		return canonicalBoolValue(v, r.bundle.GatewayPresent)
	case "gateway_name", "gateway.name":
		return r.bundle.GatewayName, isStringLike(v)
	case "cert_store", "claude_code_cert_store":
		return r.bundle.CertStore, isStringLike(v)
	case "extra_ca_certs_present", "extra_ca_certs.present", "node_extra_ca_certs_present":
		return canonicalBoolValue(v, r.bundle.ExtraCACertsPresent)
	case "mtls_enabled", "mtls.enabled":
		return canonicalBoolValue(v, r.bundle.MTLSEnabled)
	case "webfetch_preflight_enabled", "webfetch.preflight_enabled":
		return canonicalBoolValue(v, r.bundle.WebFetchPreflightEnabled)
	case "skip_webfetch_preflight":
		return canonicalBoolValue(v, !r.bundle.WebFetchPreflightEnabled)
	case "nonessential_traffic_disabled", "disable_nonessential_traffic":
		return canonicalBoolValue(v, r.bundle.NonessentialTrafficDisabled)
	case "telemetry_disabled", "disable_telemetry", "otel_sdk_disabled", "otel.sdk_disabled":
		return canonicalBoolValue(v, r.bundle.TelemetryDisabled)
	case "error_reporting_disabled", "disable_error_reporting":
		return canonicalBoolValue(v, r.bundle.ErrorReportingDisabled)
	case "mcp_proxy_mode":
		return r.bundle.MCPProxyMode, isStringLike(v)
	case "direct_connection_policy":
		return r.bundle.DirectConnectionPolicy, isStringLike(v)
	case "statsig_stable_id", "statsig.stable_id", "statsig.stableid", "stable_id":
		return r.bundle.StatsigStableID, isStringLike(v)
	case "statsig_user_id", "statsig.user_id":
		return r.bundle.StatsigUserID, isStringLike(v)
	case "feature_gate_seed", "feature.gate.seed":
		return r.bundle.FeatureGateSeed, isStringLike(v)
	case "feature_gates", "feature.gates", "statsig.feature_gates":
		return boolMapToAny(r.bundle.FeatureGates), isObjectLike(v)
	case "experiment_groups", "experiment.groups", "statsig.experiment_groups":
		return stringMapToAny(r.bundle.ExperimentGroups), isObjectLike(v)
	default:
		return nil, false
	}
}

func normalizeIdentityKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	key = strings.Trim(key, `"`)
	key = strings.ToLower(key)
	key = strings.ReplaceAll(key, "-", "_")
	key = strings.ReplaceAll(key, " ", "_")
	return key
}

func joinIdentityKey(parent, child string) string {
	if parent == "" {
		return child
	}
	if child == "" {
		return parent
	}
	return parent + "." + child
}

func isStringLike(v any) bool {
	_, ok := v.(string)
	return ok || v == nil
}

func isArrayLike(v any) bool {
	_, ok := v.([]any)
	return ok || v == nil
}

func isObjectLike(v any) bool {
	_, ok := v.(map[string]any)
	return ok || v == nil
}

func canonicalBoolValue(v any, value bool) (any, bool) {
	switch v.(type) {
	case bool:
		return value, true
	case string:
		return strconv.FormatBool(value), true
	case nil:
		return value, true
	default:
		return nil, false
	}
}

func stringSliceToAny(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

func boolMapToAny(in map[string]bool) map[string]any {
	out := make(map[string]any, len(in))
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out[k] = in[k]
	}
	return out
}

func stringMapToAny(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out[k] = in[k]
	}
	return out
}

func jsonValuesEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func sortReplacements(repls []identityReplacement) {
	sort.SliceStable(repls, func(i, j int) bool {
		return len(repls[i].from) > len(repls[j].from)
	})
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func replaceToken(s, old, next string) string {
	if old == "" || old == next || !strings.Contains(s, old) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for {
		idx := strings.Index(s[last:], old)
		if idx < 0 {
			b.WriteString(s[last:])
			break
		}
		start := last + idx
		end := start + len(old)
		if tokenBoundary(s, start-1) && tokenBoundary(s, end) {
			b.WriteString(s[last:start])
			b.WriteString(next)
			last = end
			continue
		}
		b.WriteString(s[last:end])
		last = end
	}
	return b.String()
}

func tokenBoundary(s string, idx int) bool {
	if idx < 0 || idx >= len(s) {
		return true
	}
	r := rune(s[idx])
	return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' || r == '@')
}

func cleanUserToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if idx := strings.LastIndexAny(s, `/\`); idx >= 0 {
		s = s[idx+1:]
	}
	if idx := strings.IndexByte(s, '@'); idx > 0 {
		s = s[:idx]
	}
	return cleanToken(s, '_')
}

func cleanHostToken(s string) string {
	return strings.Trim(cleanToken(strings.ToLower(strings.TrimSpace(s)), '-'), "-.")
}

func cleanToken(s string, fallback rune) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '.', r == '-', r == '_':
			b.WriteRune(r)
		case fallback != 0:
			b.WriteRune(fallback)
		}
	}
	return strings.Trim(b.String(), "._-")
}

func defaultShell() string {
	if runtime.GOOS == "windows" {
		return `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	}
	return "/bin/bash"
}

func detectOSVersion() string {
	for _, path := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(string(b), "\n")
		for _, key := range []string{"VERSION_ID", "BUILD_ID", "VERSION"} {
			for _, line := range lines {
				k, v, ok := strings.Cut(line, "=")
				if !ok || k != key {
					continue
				}
				v = strings.Trim(v, `"' `)
				if v != "" {
					return cleanToken(v, '.')
				}
			}
		}
	}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		if v := cleanToken(strings.TrimSpace(string(b)), '.'); v != "" {
			return v
		}
	}
	return runtime.GOOS
}

func detectPrimaryIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err == nil {
		defer conn.Close()
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok && addr.IP != nil {
			if ip := addr.IP.String(); ip != "" {
				return ip
			}
		}
	}
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				var ip net.IP
				switch t := addr.(type) {
				case *net.IPNet:
					ip = t.IP
				case *net.IPAddr:
					ip = t.IP
				}
				if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
					continue
				}
				if v4 := ip.To4(); v4 != nil {
					return v4.String()
				}
				if ip.To16() != nil {
					return ip.String()
				}
			}
		}
	}
	return "127.0.0.1"
}

func detectDNSResolvers() []string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return []string{"1.1.1.1"}
	}
	var resolvers []string
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		host := strings.TrimSpace(fields[1])
		if ip := net.ParseIP(host); ip != nil {
			resolvers = append(resolvers, ip.String())
		}
	}
	resolvers = normalizeDNSResolvers(resolvers)
	if len(resolvers) == 0 {
		return []string{"1.1.1.1"}
	}
	return resolvers
}

func normalizeDNSResolvers(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, resolver := range in {
		resolver = strings.TrimSpace(resolver)
		if resolver == "" {
			continue
		}
		if ip := net.ParseIP(resolver); ip != nil {
			resolver = ip.String()
		}
		if seen[resolver] {
			continue
		}
		seen[resolver] = true
		out = append(out, resolver)
	}
	return out
}

func detectDNSProvider(resolvers []string) string {
	resolvers = normalizeDNSResolvers(resolvers)
	if len(resolvers) == 0 {
		return "resolver-local"
	}
	names := map[string]string{
		"1.1.1.1":              "cloudflare",
		"1.0.0.1":              "cloudflare",
		"2606:4700:4700::1111": "cloudflare",
		"2606:4700:4700::1001": "cloudflare",
		"8.8.8.8":              "google",
		"8.8.4.4":              "google",
		"2001:4860:4860::8888": "google",
		"2001:4860:4860::8844": "google",
		"9.9.9.9":              "quad9",
		"149.112.112.112":      "quad9",
		"208.67.222.222":       "opendns",
		"208.67.220.220":       "opendns",
	}
	if name, ok := names[resolvers[0]]; ok {
		return name
	}
	if strings.HasPrefix(resolvers[0], "127.") || resolvers[0] == "::1" {
		return "resolver-local"
	}
	return "resolver-" + shortHash(strings.Join(resolvers, ","), 10)
}

type proxyFingerprint struct {
	ProxyPresent                bool
	ProxyType                   string
	HTTPProxy                   string
	HTTPSProxy                  string
	AllProxy                    string
	NoProxy                     string
	AnthropicBaseURL            string
	GatewayPresent              bool
	GatewayName                 string
	CertStore                   string
	ExtraCACertsPresent         bool
	MTLSEnabled                 bool
	WebFetchPreflightEnabled    bool
	NonessentialTrafficDisabled bool
	TelemetryDisabled           bool
	ErrorReportingDisabled      bool
	MCPProxyMode                string
	DirectConnectionPolicy      string
}

func detectProxyFingerprint() proxyFingerprint {
	return directProxyFingerprint()
}

func normalizeProxyFields(bundle IdentityBundle) proxyFingerprint {
	return directProxyFingerprint()
}

func directProxyFingerprint() proxyFingerprint {
	return proxyFingerprint{
		ProxyPresent:                false,
		ProxyType:                   "direct",
		HTTPProxy:                   "",
		HTTPSProxy:                  "",
		AllProxy:                    "",
		NoProxy:                     "",
		AnthropicBaseURL:            "https://api.anthropic.com",
		GatewayPresent:              false,
		GatewayName:                 "",
		CertStore:                   "system",
		ExtraCACertsPresent:         false,
		MTLSEnabled:                 false,
		WebFetchPreflightEnabled:    true,
		NonessentialTrafficDisabled: false,
		TelemetryDisabled:           false,
		ErrorReportingDisabled:      false,
		MCPProxyMode:                "direct",
		DirectConnectionPolicy:      "direct",
	}
}

func sanitizeProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	candidate := raw
	if !strings.Contains(candidate, "://") {
		candidate = "http://" + candidate
	}
	u, err := url.Parse(candidate)
	if err != nil || u.Host == "" {
		return stripURLCredentials(raw)
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func sanitizeBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	candidate := raw
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + candidate
	}
	u, err := url.Parse(candidate)
	if err != nil || u.Host == "" {
		return stripURLCredentials(raw)
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/")
}

func stripURLCredentials(raw string) string {
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		prefix := raw[:at]
		if strings.Contains(prefix, "://") || strings.Contains(prefix, ":") {
			return raw[at+1:]
		}
	}
	return raw
}

func normalizeNoProxy(raw string) string {
	parts := strings.Split(raw, ",")
	var out []string
	seen := map[string]bool{}
	for _, part := range parts {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func deriveProxyType(values ...string) string {
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" {
			continue
		}
		scheme := "http"
		if u, err := url.Parse(value); err == nil && u.Scheme != "" {
			scheme = strings.ToLower(u.Scheme)
		}
		if strings.HasPrefix(scheme, "socks") {
			scheme = "socks"
		}
		seen[scheme] = true
	}
	if len(seen) == 0 {
		return "direct"
	}
	if len(seen) > 1 {
		return "mixed"
	}
	for scheme := range seen {
		return scheme
	}
	return "direct"
}

func isDefaultAnthropicBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "" || host == "api.anthropic.com" || host == "console.anthropic.com"
}

func deriveGatewayName(baseURL string, present bool) string {
	if !present || baseURL == "" {
		return ""
	}
	if u, err := url.Parse(baseURL); err == nil && u.Hostname() != "" {
		host := cleanHostToken(u.Hostname())
		if host != "" {
			return "gateway-" + shortHash(host, 10)
		}
	}
	return "gateway-" + shortHash(baseURL, 10)
}

func deriveMCPProxyMode(p proxyFingerprint) string {
	switch {
	case p.GatewayPresent:
		return "base_url_gateway"
	case p.ProxyPresent:
		return "env_proxy"
	default:
		return "direct"
	}
}

func deriveDirectConnectionPolicy(p proxyFingerprint) string {
	switch {
	case p.GatewayPresent && p.ProxyPresent:
		return "gateway_proxy"
	case p.GatewayPresent:
		return "gateway"
	case p.ProxyPresent:
		return "proxy_preferred"
	default:
		return "direct"
	}
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

func envBool(keys ...string) (bool, bool) {
	for _, key := range keys {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			continue
		}
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on", "enabled":
			return true, true
		case "0", "false", "no", "off", "disabled":
			return false, true
		default:
			return true, true
		}
	}
	return false, false
}

func detectClaudeSettingBool(key string) (bool, bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false, false
	}
	for _, path := range []string{
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(home, ".claude.json"),
	} {
		b, err := os.ReadFile(path)
		if err != nil || len(b) == 0 {
			continue
		}
		var v any
		if json.Unmarshal(b, &v) != nil {
			continue
		}
		if got, ok := findBoolSetting(v, key); ok {
			return got, true
		}
	}
	return false, false
}

func findBoolSetting(v any, key string) (bool, bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == key {
				switch b := val.(type) {
				case bool:
					return b, true
				case string:
					parsed, err := strconv.ParseBool(strings.ToLower(strings.TrimSpace(b)))
					return parsed, err == nil
				}
			}
			if got, ok := findBoolSetting(val, key); ok {
				return got, true
			}
		}
	case []any:
		for _, item := range t {
			if got, ok := findBoolSetting(item, key); ok {
				return got, true
			}
		}
	}
	return false, false
}

func detectTimezone() string {
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" {
		return tz
	}
	if name, _ := time.Now().Zone(); strings.TrimSpace(name) != "" {
		return name
	}
	return "UTC"
}

func detectRegion(timezone, hash string) string {
	for _, key := range []string{"AWS_REGION", "AWS_DEFAULT_REGION", "GOOGLE_CLOUD_REGION", "AZURE_REGION", "REGION"} {
		if v := cleanToken(strings.ToLower(strings.TrimSpace(os.Getenv(key))), '-'); v != "" {
			return v
		}
	}
	tz := strings.ToLower(strings.TrimSpace(timezone))
	switch {
	case strings.Contains(tz, "america") || strings.Contains(tz, "est") || strings.Contains(tz, "cst") || strings.Contains(tz, "mst") || strings.Contains(tz, "pst"):
		return "us-" + []string{"east", "central", "west"}[hashIndex(hash, 3)]
	case strings.Contains(tz, "europe") || strings.Contains(tz, "cet") || strings.Contains(tz, "gmt"):
		return "eu-" + []string{"west", "central", "north"}[hashIndex(hash, 3)]
	case strings.Contains(tz, "asia"):
		return "ap-" + []string{"east", "south", "northeast"}[hashIndex(hash, 3)]
	}
	if hash == "" {
		hash = shortHash(timezone, 8)
	}
	return "region-" + hash[:6]
}

func deterministicASN(hash string) string {
	if len(hash) < 8 {
		hash = shortHash(hash, 8)
	}
	value := 64512 + int(hashByte(hash, 0))<<8 + int(hashByte(hash, 1))
	return fmt.Sprintf("AS%d", value)
}

func deterministicFeatureGates(hash string) map[string]bool {
	keys := []string{
		"claude_code_plan_mode",
		"claude_code_multi_agent",
		"claude_code_hooks",
		"claude_code_background_bash",
		"claude_code_mcp",
		"claude_code_telemetry_v2",
		"claude_code_thinking",
		"claude_code_checkpointing",
	}
	out := make(map[string]bool, len(keys))
	for i, key := range keys {
		out[key] = hashByte(hash, i)%2 == 0
	}
	return out
}

func deterministicExperimentGroups(hash string) map[string]string {
	groups := []string{"control", "variant_a", "variant_b"}
	keys := []string{"claude_code_ui", "tool_scheduler", "telemetry_pipeline", "mcp_registry"}
	out := make(map[string]string, len(keys))
	for i, key := range keys {
		out[key] = groups[hashIndex(hash[i:], len(groups))]
	}
	return out
}

func hashIndex(hash string, n int) int {
	if n <= 0 {
		return 0
	}
	return int(hashByte(hash, 0)) % n
}

func hashByte(hash string, idx int) byte {
	if len(hash) < 2 {
		hash = shortHash(hash, 64)
	}
	pos := (idx * 2) % len(hash)
	if pos+2 > len(hash) {
		pos = 0
	}
	b, err := hex.DecodeString(hash[pos : pos+2])
	if err != nil || len(b) == 0 {
		return byte(pos)
	}
	return b[0]
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func shortHash(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	h := hex.EncodeToString(sum[:])
	if n > len(h) {
		n = len(h)
	}
	return h[:n]
}
