// Package netguard provides outbound HTTP clients that cannot be steered into
// the bridge's own network.
//
// Handles, did:web documents and PDS endpoints are chosen by whoever logs in,
// so every outbound request is treated as attacker-directed. The guard checks
// the IP address actually being dialled (after DNS resolution, at connect
// time), which defeats DNS rebinding and redirects to internal hosts alike.
package netguard

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrBlocked is returned when a destination address is not publicly routable.
var ErrBlocked = errors.New("netguard: destination address is not allowed")

// ErrInsecureScheme is returned for non-HTTPS requests to hosts not on the
// plain-HTTP allow list.
var ErrInsecureScheme = errors.New("netguard: only https is allowed")

// ErrTooLarge is returned when a response body exceeds the configured limit.
var ErrTooLarge = errors.New("netguard: response body too large")

// blocked lists every range that must never be dialled.
var blocked = mustPrefixes(
	// IPv4
	"0.0.0.0/8",          // "this" network, unspecified
	"10.0.0.0/8",         // RFC 1918
	"100.64.0.0/10",      // CGNAT, and Tailscale
	"127.0.0.0/8",        // loopback
	"169.254.0.0/16",     // link-local, cloud metadata
	"172.16.0.0/12",      // RFC 1918
	"192.0.0.0/24",       // IETF protocol assignments
	"192.0.2.0/24",       // TEST-NET-1
	"192.88.99.0/24",     // 6to4 relay anycast
	"192.168.0.0/16",     // RFC 1918
	"198.18.0.0/15",      // benchmarking
	"198.51.100.0/24",    // TEST-NET-2
	"203.0.113.0/24",     // TEST-NET-3
	"224.0.0.0/4",        // multicast
	"240.0.0.0/4",        // reserved
	"255.255.255.255/32", // broadcast
	// IPv6
	"::/128",        // unspecified
	"::1/128",       // loopback
	"::ffff:0:0/96", // IPv4-mapped (checked after unmapping as well)
	"64:ff9b::/96",  // NAT64, can reach internal IPv4
	"64:ff9b:1::/48",
	"100::/64",      // discard
	"2001::/32",     // Teredo
	"2001:db8::/32", // documentation
	"2002::/16",     // 6to4, embeds IPv4
	"fc00::/7",      // unique local (includes Tailscale fd7a:115c:a1e0::/48)
	"fe80::/10",     // link-local
	"fec0::/10",     // deprecated site-local
	"ff00::/8",      // multicast
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// IsPublic reports whether addr is a globally routable unicast address.
func IsPublic(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	if addr.Zone() != "" {
		return false
	}
	for _, p := range blocked {
		if p.Contains(addr) {
			return false
		}
	}
	return addr.IsGlobalUnicast()
}

// Options configure a guarded client.
type Options struct {
	// Timeout bounds the whole request including reading the body.
	Timeout time.Duration
	// MaxBodyBytes caps response bodies; reads past it fail with ErrTooLarge.
	MaxBodyBytes int64
	// MaxRedirects caps redirect hops (0 disables redirects).
	MaxRedirects int
	// PlainHTTPHosts may be fetched over plain http (e.g. a configured PLC mirror).
	PlainHTTPHosts []string
	// AllowPrivate disables the address check. Development and tests only.
	AllowPrivate bool
	// AllowAddrs permits specific ip:port destinations despite the check (tests).
	AllowAddrs []string
	// Resolver overrides DNS resolution (tests).
	Resolver func(ctx context.Context, host string) ([]netip.Addr, error)
	// MaxConnsPerHost bounds concurrent connections to one host.
	MaxConnsPerHost int
	// UserAgent is set on every request that lacks one.
	UserAgent string
	// TLSConfig overrides the TLS client configuration (tests).
	TLSConfig *tls.Config
}

// Guard dials only public addresses.
type Guard struct {
	opts   Options
	allow  map[string]bool
	dialer *net.Dialer
}

// NewGuard builds a dialer guard.
func NewGuard(opts Options) *Guard {
	g := &Guard{opts: opts, allow: map[string]bool{}}
	for _, a := range opts.AllowAddrs {
		g.allow[a] = true
	}
	g.dialer = &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   g.control,
	}
	return g
}

// control runs after resolution, immediately before connect, with the literal
// address being dialled. This is the check that cannot be raced by DNS.
func (g *Guard) control(network, address string, _ syscall.RawConn) error {
	if g.opts.AllowPrivate || g.allow[address] {
		return nil
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrBlocked, address)
	}
	if !IsPublic(ap.Addr()) {
		return fmt.Errorf("%w: %s", ErrBlocked, ap.Addr())
	}
	return nil
}

// DialContext resolves host itself so every candidate address is checked,
// then dials the chosen literal address (which control checks again).
func (g *Guard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else if g.opts.Resolver != nil {
		ips, err = g.opts.Resolver(ctx, host)
		if err != nil {
			return nil, err
		}
	} else {
		ips, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
	}
	var lastErr error = fmt.Errorf("%w: no usable address for %s", ErrBlocked, host)
	for _, ip := range ips {
		target := net.JoinHostPort(ip.Unmap().String(), port)
		if !g.opts.AllowPrivate && !g.allow[target] && !IsPublic(ip) {
			lastErr = fmt.Errorf("%w: %s resolved to %s", ErrBlocked, host, ip)
			continue
		}
		c, err := g.dialer.DialContext(ctx, network, target)
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// NewClient returns an HTTP client whose every connection passes the guard.
func NewClient(opts Options) *http.Client {
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.MaxBodyBytes == 0 {
		opts.MaxBodyBytes = 1 << 20
	}
	if opts.MaxConnsPerHost == 0 {
		opts.MaxConnsPerHost = 64
	}
	g := NewGuard(opts)
	tr := &http.Transport{
		Proxy:                  nil, // never honour HTTP(S)_PROXY for attacker-chosen hosts
		DialContext:            g.DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           512,
		MaxIdleConnsPerHost:    16,
		MaxConnsPerHost:        opts.MaxConnsPerHost,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  opts.Timeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		TLSClientConfig:        opts.TLSConfig,
	}
	plain := map[string]bool{}
	for _, h := range opts.PlainHTTPHosts {
		plain[strings.ToLower(h)] = true
	}
	rt := &guardedTransport{next: tr, plain: plain, maxBody: opts.MaxBodyBytes, ua: opts.UserAgent}
	return &http.Client{
		Transport: rt,
		Timeout:   opts.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > opts.MaxRedirects {
				return fmt.Errorf("netguard: stopped after %d redirects", opts.MaxRedirects)
			}
			return rt.checkURL(req.URL)
		},
	}
}

type guardedTransport struct {
	next    http.RoundTripper
	plain   map[string]bool
	maxBody int64
	ua      string
}

func (t *guardedTransport) checkURL(u *url.URL) error {
	switch u.Scheme {
	case "https":
	case "http":
		if !t.plain[strings.ToLower(u.Host)] && !t.plain[strings.ToLower(u.Hostname())] {
			return fmt.Errorf("%w: %s", ErrInsecureScheme, u.Redacted())
		}
	default:
		return fmt.Errorf("%w: scheme %q", ErrInsecureScheme, u.Scheme)
	}
	if u.User != nil {
		return errors.New("netguard: credentials in URL are not allowed")
	}
	return nil
}

func (t *guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.checkURL(req.URL); err != nil {
		return nil, err
	}
	if t.ua != "" && req.Header.Get("User-Agent") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", t.ua)
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > t.maxBody {
		resp.Body.Close()
		return nil, ErrTooLarge
	}
	resp.Body = &limitedBody{rc: resp.Body, remaining: t.maxBody}
	return resp, nil
}

type limitedBody struct {
	rc        io.ReadCloser
	remaining int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		// Probe for one more byte to distinguish "exactly at limit" from "over".
		var one [1]byte
		n, err := b.rc.Read(one[:])
		if n > 0 {
			return 0, ErrTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.rc.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }
