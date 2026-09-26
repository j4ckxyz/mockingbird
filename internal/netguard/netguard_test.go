package netguard

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsPublic(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8":              true,
		"1.1.1.1":              true,
		"2606:4700:4700::1111": true,
		"127.0.0.1":            false,
		"127.255.255.254":      false,
		"10.1.2.3":             false,
		"172.16.0.1":           false,
		"172.31.255.255":       false,
		"192.168.1.1":          false,
		"169.254.169.254":      false,
		"100.64.0.1":           false,
		"100.100.100.100":      false, // Tailscale MagicDNS
		"100.127.255.255":      false,
		"0.0.0.0":              false,
		"224.0.0.1":            false,
		"255.255.255.255":      false,
		"::":                   false,
		"::1":                  false,
		"::ffff:127.0.0.1":     false,
		"::ffff:10.0.0.1":      false,
		"::ffff:8.8.8.8":       true,
		"fe80::1":              false,
		"fc00::1":              false,
		"fd7a:115c:a1e0::1":    false, // Tailscale IPv6
		"ff02::1":              false,
		"64:ff9b::7f00:1":      false, // NAT64 of 127.0.0.1
		"2002:7f00:1::1":       false, // 6to4 of 127.0.0.1
		"2001:db8::1":          false,
	}
	for s, want := range cases {
		if got := IsPublic(netip.MustParseAddr(s)); got != want {
			t.Errorf("IsPublic(%s) = %v, want %v", s, got, want)
		}
	}
}

func newTLSServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewTLSServer(h)
	t.Cleanup(s.Close)
	return s
}

func clientFor(t *testing.T, opts Options, servers ...*httptest.Server) *http.Client {
	t.Helper()
	opts.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	for _, s := range servers {
		opts.AllowAddrs = append(opts.AllowAddrs, s.Listener.Addr().String())
	}
	return NewClient(opts)
}

func TestBlocksLoopbackDirectly(t *testing.T) {
	s := newTLSServer(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secret") })
	c := clientFor(t, Options{}) // server not allow-listed
	_, err := c.Get(s.URL)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("want ErrBlocked, got %v", err)
	}
}

func TestBlocksLocalhostName(t *testing.T) {
	s := newTLSServer(t, func(w http.ResponseWriter, r *http.Request) {})
	_, port, _ := net.SplitHostPort(s.Listener.Addr().String())
	c := clientFor(t, Options{})
	_, err := c.Get("https://localhost:" + port + "/")
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("want ErrBlocked, got %v", err)
	}
}

// TestDNSRebinding simulates a hostname that first resolves to an allowed
// address and then, on the next lookup, to an internal one.
func TestDNSRebinding(t *testing.T) {
	s := newTLSServer(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	ap := netip.MustParseAddrPort(s.Listener.Addr().String())
	var calls atomic.Int32
	resolver := func(ctx context.Context, host string) ([]netip.Addr, error) {
		if calls.Add(1) == 1 {
			return []netip.Addr{ap.Addr()}, nil // "public" (allow-listed) first
		}
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	}
	c := clientFor(t, Options{Resolver: resolver}, s)
	c.Transport.(*guardedTransport).next.(*http.Transport).DisableKeepAlives = true
	url := "https://rebind.example:" + itoa(ap.Port()) + "/"
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("first request should succeed: %v", err)
	}
	resp.Body.Close()
	_, err = c.Get(url)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("second request after rebinding: want ErrBlocked, got %v", err)
	}
}

// TestMixedResolution ensures an internal address in a multi-address answer
// is skipped rather than dialled.
func TestMixedResolution(t *testing.T) {
	var hit atomic.Bool
	internal := newTLSServer(t, func(w http.ResponseWriter, r *http.Request) { hit.Store(true) })
	iap := netip.MustParseAddrPort(internal.Listener.Addr().String())
	resolver := func(ctx context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.0.0.1"), iap.Addr()}, nil
	}
	c := clientFor(t, Options{Resolver: resolver})
	_, err := c.Get("https://mixed.example:" + itoa(iap.Port()) + "/")
	if !errors.Is(err, ErrBlocked) || hit.Load() {
		t.Fatalf("want ErrBlocked and no hit, got err=%v hit=%v", err, hit.Load())
	}
}

func TestRedirectToInternalBlocked(t *testing.T) {
	var hit atomic.Bool
	internal := newTLSServer(t, func(w http.ResponseWriter, r *http.Request) { hit.Store(true) })
	for name, target := range map[string]string{
		"https-loopback": internal.URL + "/admin",
		"metadata":       "https://169.254.169.254/latest/meta-data/",
		"mapped-v6":      "https://[::ffff:127.0.0.1]:" + itoa(netip.MustParseAddrPort(internal.Listener.Addr().String()).Port()) + "/",
		"plain-http":     "http://example.com/",
		"file-scheme":    "file:///etc/passwd",
	} {
		t.Run(name, func(t *testing.T) {
			front := newTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target, http.StatusFound)
			})
			c := clientFor(t, Options{MaxRedirects: 3}, front)
			_, err := c.Get(front.URL)
			if err == nil {
				t.Fatal("redirect should have been refused")
			}
			if !errors.Is(err, ErrBlocked) && !errors.Is(err, ErrInsecureScheme) {
				t.Fatalf("unexpected error: %v", err)
			}
			if hit.Load() {
				t.Fatal("internal server was reached")
			}
		})
	}
}

func TestRedirectLimit(t *testing.T) {
	var s *httptest.Server
	s = newTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.URL+"/again", http.StatusFound)
	})
	c := clientFor(t, Options{MaxRedirects: 2}, s)
	_, err := c.Get(s.URL)
	if err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("want redirect limit error, got %v", err)
	}
}

func TestPlainHTTPRefused(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer s.Close()
	c := clientFor(t, Options{}, s)
	_, err := c.Get(s.URL)
	if !errors.Is(err, ErrInsecureScheme) {
		t.Fatalf("want ErrInsecureScheme, got %v", err)
	}
	// Allowed when the host is explicitly configured (e.g. a PLC mirror).
	c = clientFor(t, Options{PlainHTTPHosts: []string{s.Listener.Addr().String()}}, s)
	resp, err := c.Get(s.URL)
	if err != nil {
		t.Fatalf("allow-listed plain host: %v", err)
	}
	resp.Body.Close()
}

func TestBodyLimit(t *testing.T) {
	s := newTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		// No Content-Length: streamed body.
		for i := 0; i < 100; i++ {
			io.WriteString(w, strings.Repeat("x", 1000))
			w.(http.Flusher).Flush()
		}
	})
	c := clientFor(t, Options{MaxBodyBytes: 10_000}, s)
	resp, err := c.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestTimeout(t *testing.T) {
	s := newTLSServer(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) })
	c := clientFor(t, Options{Timeout: 200 * time.Millisecond}, s)
	start := time.Now()
	_, err := c.Get(s.URL)
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("want fast timeout, got err=%v after %v", err, time.Since(start))
	}
}

func itoa(p uint16) string { return strconv.Itoa(int(p)) }
