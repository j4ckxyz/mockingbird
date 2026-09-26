// Package config loads bridge configuration from environment variables.
//
// Every variable is prefixed MB_. Secrets may also be supplied through a
// file by appending _FILE to the variable name (for Docker secrets).
package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the full runtime configuration.
type Config struct {
	// Listeners
	HTTPAddr      string   // plain HTTP listener, e.g. ":8080"
	LegacyTLSAddr string   // optional TLS 1.0 listener, e.g. ":8443"; empty disables
	AdminAddrs    []string // admin/metrics listeners; loopback or Tailscale only

	// Public identity of the bridge
	PublicURL   *url.URL // base URL used in generated links (images, post pages)
	BridgeHosts []string // hostnames that serve the bridge's own pages and API
	SearchHosts []string // hostnames treated as the Search API (search.twitter.com)

	DataDir string

	// Secrets (32 bytes each)
	SecretKey     []byte // HMAC key for session keys, image signatures, OAuth tokens
	EncryptionKey []byte // AES-256-GCM key for tokens at rest

	// Upstream
	PLCURL             string
	AppViewProxy       string // atproto-proxy value for app.bsky.* calls
	ChatProxy          string // atproto-proxy value for chat.bsky.* calls
	PublicAppViewURL   string // unauthenticated AppView for post pages and anonymous search
	ImageCDNURL        string
	PublicTimelineFeed string // feed generator AT-URI used for statuses/public_timeline
	DefaultHandleHost  string // suffix appended to dotless handles at login, e.g. "bsky.social"

	// Networking
	TrustedProxies []netip.Prefix // peers whose X-Forwarded-For / X-Forwarded-Proto are trusted

	// Caches and limits
	ImageCacheBytes   int64
	ResponseCacheTTL  time.Duration
	ProfileCacheTTL   time.Duration
	IdentityCacheTTL  time.Duration
	MaxUpstreamPages  int
	PerIPRate         float64 // requests per second
	PerIPBurst        int
	PerAccountRate    float64
	PerAccountBurst   int
	ReportedRateLimit int // hourly_limit reported to clients

	// OAuth
	OAuthConsumers       map[string]string // consumer key -> consumer secret (verified clients)
	OAuthTimestampWindow time.Duration     // 0 disables timestamp checks
	OAuthRequestTokenTTL time.Duration
	SessionIdleExpiry    time.Duration

	// Legacy TLS
	TLSExtraHosts      []string
	TLSNameConstraints bool
	TLSSHA1            bool
	TLSCommonName      string

	// Dev and test
	InsecureAllowPrivateNetworks bool // disables SSRF guard for private ranges; never in production
	LogLevel                     string
}

// Load reads configuration from the environment.
func Load() (*Config, error) {
	return load(os.Getenv)
}

func load(getenv func(string) string) (*Config, error) {
	e := env{get: getenv}
	c := &Config{
		HTTPAddr:                     e.str("MB_HTTP_ADDR", ":8080"),
		LegacyTLSAddr:                e.str("MB_LEGACY_TLS_ADDR", ""),
		AdminAddrs:                   e.list("MB_ADMIN_ADDR", "127.0.0.1:9090"),
		DataDir:                      e.str("MB_DATA_DIR", "/data"),
		PLCURL:                       strings.TrimRight(e.str("MB_PLC_URL", "https://plc.directory"), "/"),
		AppViewProxy:                 e.str("MB_APPVIEW_PROXY", "did:web:api.bsky.app#bsky_appview"),
		ChatProxy:                    e.str("MB_CHAT_PROXY", "did:web:api.bsky.chat#bsky_chat"),
		PublicAppViewURL:             strings.TrimRight(e.str("MB_PUBLIC_APPVIEW_URL", "https://public.api.bsky.app"), "/"),
		ImageCDNURL:                  strings.TrimRight(e.str("MB_IMAGE_CDN_URL", "https://cdn.bsky.app"), "/"),
		PublicTimelineFeed:           e.str("MB_PUBLIC_TIMELINE_FEED", "at://did:plc:z72i7hdynmk6r22z27h6tvur/app.bsky.feed.generator/whats-hot"),
		DefaultHandleHost:            strings.Trim(e.str("MB_DEFAULT_HANDLE_HOST", "bsky.social"), "."),
		SearchHosts:                  e.list("MB_SEARCH_HOSTS", "search.twitter.com"),
		ImageCacheBytes:              int64(e.int("MB_IMAGE_CACHE_MB", 512)) << 20,
		ResponseCacheTTL:             e.dur("MB_RESPONSE_CACHE_TTL", 20*time.Second),
		ProfileCacheTTL:              e.dur("MB_PROFILE_CACHE_TTL", 5*time.Minute),
		IdentityCacheTTL:             e.dur("MB_IDENTITY_CACHE_TTL", 6*time.Hour),
		MaxUpstreamPages:             e.int("MB_MAX_UPSTREAM_PAGES", 6),
		PerIPRate:                    e.float("MB_PER_IP_RATE", 5),
		PerIPBurst:                   e.int("MB_PER_IP_BURST", 60),
		PerAccountRate:               e.float("MB_PER_ACCOUNT_RATE", 1),
		PerAccountBurst:              e.int("MB_PER_ACCOUNT_BURST", 40),
		ReportedRateLimit:            e.int("MB_REPORTED_RATE_LIMIT", 350),
		OAuthTimestampWindow:         e.dur("MB_OAUTH_TIMESTAMP_WINDOW", time.Hour),
		OAuthRequestTokenTTL:         e.dur("MB_OAUTH_REQUEST_TOKEN_TTL", 15*time.Minute),
		SessionIdleExpiry:            e.dur("MB_SESSION_IDLE_EXPIRY", 60*24*time.Hour),
		TLSExtraHosts:                e.list("MB_TLS_EXTRA_HOSTS", "twitter.com,www.twitter.com,api.twitter.com,search.twitter.com"),
		TLSNameConstraints:           e.bool("MB_TLS_NAME_CONSTRAINTS", true),
		TLSSHA1:                      e.bool("MB_TLS_SHA1", false),
		InsecureAllowPrivateNetworks: e.bool("MB_INSECURE_ALLOW_PRIVATE_NETWORKS", false),
		LogLevel:                     e.str("MB_LOG_LEVEL", "info"),
	}

	pub := e.str("MB_PUBLIC_URL", "http://localhost:8080")
	u, err := url.Parse(strings.TrimRight(pub, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		e.errs = append(e.errs, fmt.Errorf("MB_PUBLIC_URL must be an absolute http(s) URL"))
	} else {
		c.PublicURL = u
	}
	c.BridgeHosts = e.list("MB_BRIDGE_HOSTS", "")
	if len(c.BridgeHosts) == 0 && c.PublicURL != nil {
		c.BridgeHosts = []string{c.PublicURL.Hostname()}
	}
	c.TLSCommonName = e.str("MB_TLS_COMMON_NAME", "")
	if c.TLSCommonName == "" && c.PublicURL != nil {
		c.TLSCommonName = c.PublicURL.Hostname()
	}

	c.SecretKey = e.key("MB_SECRET_KEY")
	c.EncryptionKey = e.key("MB_ENCRYPTION_KEY")
	if c.SecretKey != nil && c.EncryptionKey != nil && string(c.SecretKey) == string(c.EncryptionKey) {
		e.errs = append(e.errs, errors.New("MB_SECRET_KEY and MB_ENCRYPTION_KEY must differ"))
	}

	for _, p := range e.list("MB_TRUSTED_PROXIES", "") {
		pfx, err := netip.ParsePrefix(p)
		if err != nil {
			addr, aerr := netip.ParseAddr(p)
			if aerr != nil {
				e.errs = append(e.errs, fmt.Errorf("MB_TRUSTED_PROXIES: bad entry %q", p))
				continue
			}
			pfx = netip.PrefixFrom(addr, addr.BitLen())
		}
		c.TrustedProxies = append(c.TrustedProxies, pfx.Masked())
	}

	// Consumer secrets are secrets, so MB_OAUTH_CONSUMERS_FILE is honoured too.
	c.OAuthConsumers = map[string]string{}
	for _, kv := range splitList(e.secret("MB_OAUTH_CONSUMERS")) {
		k, v, ok := strings.Cut(kv, ":")
		if !ok || k == "" || v == "" {
			e.errs = append(e.errs, errors.New("MB_OAUTH_CONSUMERS entries must be key:secret"))
			continue
		}
		c.OAuthConsumers[k] = v
	}

	for _, a := range c.AdminAddrs {
		if err := CheckAdminAddr(a); err != nil {
			e.errs = append(e.errs, err)
		}
	}
	if c.MaxUpstreamPages < 1 {
		c.MaxUpstreamPages = 1
	}
	if len(e.errs) > 0 {
		return nil, errors.Join(e.errs...)
	}
	return c, nil
}

// tailscaleV4 and tailscaleV6 are the address ranges Tailscale assigns.
var (
	tailscaleV4 = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// IsAdminPeer reports whether an address may reach admin endpoints:
// loopback or the Tailscale ranges.
func IsAdminPeer(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || tailscaleV4.Contains(a) || tailscaleV6.Contains(a)
}

// CheckAdminAddr refuses admin listen addresses that are not loopback or
// Tailscale. A wildcard address (":9090", "0.0.0.0:9090") is rejected.
func CheckAdminAddr(hostport string) error {
	host, _, err := splitHostPort(hostport)
	if err != nil {
		return fmt.Errorf("MB_ADMIN_ADDR %q: %v", hostport, err)
	}
	if host == "localhost" {
		return nil
	}
	a, err := netip.ParseAddr(host)
	if err != nil || !IsAdminPeer(a) {
		return fmt.Errorf("MB_ADMIN_ADDR %q must be a loopback or Tailscale (100.64.0.0/10, fd7a:115c:a1e0::/48) address", hostport)
	}
	return nil
}

func splitHostPort(hp string) (string, string, error) {
	i := strings.LastIndex(hp, ":")
	if i < 0 {
		return "", "", errors.New("missing port")
	}
	host := strings.Trim(hp[:i], "[]")
	if host == "" {
		return "", "", errors.New("wildcard address not allowed")
	}
	return host, hp[i+1:], nil
}

type env struct {
	get  func(string) string
	errs []error
}

func (e *env) raw(k string) string {
	return strings.TrimSpace(e.get(k))
}

func (e *env) str(k, def string) string {
	if v := e.raw(k); v != "" {
		return v
	}
	return def
}

func (e *env) list(k, def string) []string {
	return splitList(e.str(k, def))
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (e *env) int(k string, def int) int {
	v := e.raw(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: not an integer", k))
		return def
	}
	return n
}

func (e *env) float(k string, def float64) float64 {
	v := e.raw(k)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: not a number", k))
		return def
	}
	return f
}

func (e *env) bool(k string, def bool) bool {
	v := e.raw(k)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: not a boolean", k))
		return def
	}
	return b
}

func (e *env) dur(k string, def time.Duration) time.Duration {
	v := e.raw(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: not a duration", k))
		return def
	}
	return d
}

// secret reads k, or the file named by k_FILE.
func (e *env) secret(k string) string {
	if v := e.raw(k); v != "" {
		return v
	}
	if f := e.raw(k + "_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			e.errs = append(e.errs, fmt.Errorf("%s_FILE: %v", k, err))
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	return ""
}

// key reads a required 32-byte key encoded as hex or base64.
func (e *env) key(k string) []byte {
	v := e.secret(k)
	if v == "" {
		e.errs = append(e.errs, fmt.Errorf("%s is required (32 random bytes, hex or base64; generate with: openssl rand -hex 32)", k))
		return nil
	}
	if b, err := hex.DecodeString(v); err == nil && len(b) == 32 {
		return b
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(v); err == nil && len(b) == 32 {
			return b
		}
	}
	e.errs = append(e.errs, fmt.Errorf("%s must decode to exactly 32 bytes", k))
	return nil
}
