// Package api implements the Twitter REST API v1 and Search API surface and
// the bridge's public web pages.
package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackgilbert/mockingbird/internal/atp"
	"github.com/jackgilbert/mockingbird/internal/cache"
	"github.com/jackgilbert/mockingbird/internal/config"
	"github.com/jackgilbert/mockingbird/internal/ident"
	"github.com/jackgilbert/mockingbird/internal/logging"
	"github.com/jackgilbert/mockingbird/internal/media"
	"github.com/jackgilbert/mockingbird/internal/metrics"
	"github.com/jackgilbert/mockingbird/internal/ratelimit"
	"github.com/jackgilbert/mockingbird/internal/secret"
	"github.com/jackgilbert/mockingbird/internal/session"
	"github.com/jackgilbert/mockingbird/internal/store"
	"github.com/jackgilbert/mockingbird/internal/translate"
)

// Deps are the server's collaborators.
type Deps struct {
	Config   *config.Config
	Store    *store.Store
	Keys     *secret.Keys
	Sessions *session.Manager
	Resolver ident.Resolver
	Signer   *media.Signer
	Images   *media.Proxy
	Public   *atp.Client // unauthenticated AppView
	Metrics  *metrics.Metrics
	Logger   *slog.Logger
	// Web serves non-API pages (setup, post pages, OAuth login form, CA).
	Web http.Handler
	// CA returns the legacy TLS CA certificate (DER), if enabled.
	CA func() []byte
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Server is the public HTTP handler.
type Server struct {
	d        Deps
	cfg      *config.Config
	log      *slog.Logger
	ids      *idCache
	handles  *cache.LRU[string, string]         // did -> handle
	profiles *cache.LRU[string, *atp.Profile]   // did -> detailed profile (counts)
	short    *cache.LRU[string, *shortNames]    // viewer did -> short name map
	cursors  *cache.LRU[string, string]         // viewer|feed|statusID -> cursor
	reposts  *cache.LRU[string, string]         // repost URI -> reposted post URI
	resp     *cache.LRU[string, cachedResponse] // response cache
	gens     *cache.LRU[string, int]            // viewer did -> cache generation
	genMu    sync.Mutex
	ipLimit  *ratelimit.Limiter
	acLimit  *ratelimit.Limiter
	usage    *cache.LRU[string, *hourCount] // viewer did -> requests this hour
	unknown  *unknownLog
	oauth    *oauthStore
	routes   []*route
	sf       singleflightGroup
	now      func() time.Time
}

// cachedResponse holds a gzip-compressed body (or the raw body when it is
// too small to compress), sized exactly, so the cache stays small.
type cachedResponse struct {
	gz          []byte
	raw         []byte
	contentType string
	status      int
}

// New builds the server.
func New(d Deps) *Server {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	s := &Server{
		d:        d,
		cfg:      d.Config,
		log:      d.Logger,
		ids:      newIDCache(d.Store),
		handles:  cache.New[string, string](200_000),
		profiles: cache.New[string, *atp.Profile](50_000),
		short:    cache.New[string, *shortNames](20_000),
		cursors:  cache.New[string, string](100_000),
		reposts:  cache.New[string, string](100_000),
		resp:     cache.New[string, cachedResponse](5_000),
		gens:     cache.New[string, int](50_000),
		ipLimit:  ratelimit.NewLimiter(d.Config.PerIPRate, d.Config.PerIPBurst, 200_000),
		acLimit:  ratelimit.NewLimiter(d.Config.PerAccountRate, d.Config.PerAccountBurst, 200_000),
		usage:    cache.New[string, *hourCount](100_000),
		unknown:  newUnknownLog(d.Logger),
		oauth:    newOAuthStore(d.Config.OAuthRequestTokenTTL),
		now:      time.Now,
	}
	if d.Now != nil {
		s.now = d.Now
	}
	s.routes = s.buildRoutes()
	return s
}

// SetWeb installs the page handler (created after the server, since pages
// call back into it for OAuth and uploads).
func (s *Server) SetWeb(h http.Handler) { s.d.Web = h }

// Unknown exposes the unknown-endpoint log for the admin handler.
func (s *Server) Unknown() map[string]int { return s.unknown.Snapshot() }

// builder returns a translator bound to one viewer.
func (s *Server) builder(c *Ctx) *translate.Builder {
	b := &translate.Builder{
		IDs:     s.ids,
		Links:   s.links(),
		Handles: s.handles,
		Profiles: func(ctx context.Context, dids []string) map[string]*atp.Profile {
			return s.detailedProfiles(ctx, c, dids)
		},
	}
	if c.Sess != nil {
		sn := s.shortNamesFor(c.Sess.DID)
		b.OnProfile = func(p *atp.Profile) { sn.add(p.Handle) }
	}
	return b
}

// ServeHTTP applies the middleware chain.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := s.now()
	ctx, upstream := atp.WithUpstreamTimer(r.Context())
	r = r.WithContext(ctx)
	rw := &statusWriter{ResponseWriter: w, status: 200}
	route := "unmatched"
	defer func() {
		if p := recover(); p != nil {
			// Log the panic and stack with secrets scrubbed; never the request.
			s.log.Error("panic", "path", r.URL.Path, "panic", logging.RedactString(fmt.Sprint(p)),
				"stack", logging.RedactString(string(debug.Stack())))
			if !rw.wrote {
				http.Error(rw, "internal error", http.StatusInternalServerError)
			}
		}
		total := s.now().Sub(start)
		if s.d.Metrics != nil {
			s.d.Metrics.ObserveRequest(route, rw.status, total, total-time.Duration(upstream.Load()))
		}
		if s.log.Enabled(r.Context(), slog.LevelDebug) {
			// Path and parameter names only: query values can carry tokens.
			names := make([]string, 0, len(r.URL.Query()))
			for k := range r.URL.Query() {
				names = append(names, k)
			}
			sort.Strings(names)
			s.log.Debug("request", "method", r.Method, "host", hostOnly(r.Host), "path", r.URL.Path, "params", strings.Join(names, ","),
				"route", route, "status", rw.status, "ms", total.Milliseconds(), "upstream_ms", time.Duration(upstream.Load()).Milliseconds(),
				"user_agent", r.UserAgent())
		}
	}()

	ip := s.clientIP(r)
	if !s.ipLimit.Allow(ip) {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		route = "ratelimited"
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, s.bodyLimit(r))
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	route = s.dispatch(rw, r, ip)
}

func (s *Server) bodyLimit(r *http.Request) int64 {
	if strings.Contains(r.URL.Path, "upload") || strings.Contains(r.URL.Path, "update_profile_image") {
		return 5 << 20
	}
	return 64 << 10
}

// clientIP returns the peer address, honouring X-Forwarded-For only from
// configured trusted proxies.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	if s.trusted(peer) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			// Walk right to left past trusted hops.
			for i := len(parts) - 1; i >= 0; i-- {
				a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
				if err != nil {
					break
				}
				a = a.Unmap()
				if !s.trusted(a) || i == 0 {
					return a.String()
				}
			}
		}
	}
	return peer.String()
}

func (s *Server) trusted(a netip.Addr) bool {
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// scheme returns the scheme the client used.
func (s *Server) scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if a, err := netip.ParseAddr(host); err == nil && s.trusted(a.Unmap()) && r.Header.Get("X-Forwarded-Proto") == "https" {
		return "https"
	}
	return "http"
}

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

func (s *Server) isSearchHost(host string) bool {
	for _, h := range s.cfg.SearchHosts {
		if h == host {
			return true
		}
	}
	return false
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// gzipBody compresses a response body into an exactly sized slice, or
// returns nil when compression is not worthwhile.
func gzipBody(body []byte) []byte {
	if len(body) <= 512 {
		return nil
	}
	var buf bytes.Buffer
	zw := gzipPool.Get().(*gzip.Writer)
	zw.Reset(&buf)
	zw.Write(body)
	zw.Close()
	gzipPool.Put(zw)
	return bytes.Clone(buf.Bytes())
}

// gunzip reverses gzipBody (for cache hits from clients without gzip).
func gunzip(gz []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}

// write sends a body, gzipped when the client accepts it and it is worth it.
// Old iPhone OS networking sends Accept-Encoding: gzip, and compression is
// the biggest single saving on a home upload link. gz may carry an already
// compressed copy of body (or be nil).
func write(w http.ResponseWriter, r *http.Request, status int, contentType string, body, gz []byte) {
	h := w.Header()
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Add("Vary", "Accept-Encoding")
	if acceptsGzip(r) {
		if gz == nil && body != nil {
			gz = gzipBody(body)
		}
		if gz != nil {
			h.Set("Content-Encoding", "gzip")
			h.Set("Content-Length", fmt.Sprint(len(gz)))
			w.WriteHeader(status)
			if r.Method != http.MethodHead {
				w.Write(gz)
			}
			return
		}
	}
	if body == nil && gz != nil {
		var err error
		if body, err = gunzip(gz); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	h.Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

var gzipPool = sync.Pool{New: func() any {
	zw, _ := gzip.NewWriterLevel(io.Discard, 6)
	return zw
}}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc, q, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(enc), "gzip") && !strings.Contains(strings.ReplaceAll(q, " ", ""), "q=0") {
			return true
		}
	}
	return false
}

// shortNames maps a bare name ("alice") to full handles the viewer has seen
// recently ("alice.example.com"), for resolving dotless @mentions.
type shortNames struct {
	mu sync.Mutex
	m  *cache.LRU[string, string]
}

func (s *Server) shortNamesFor(did string) *shortNames {
	if sn, ok := s.short.Get(did); ok {
		return sn
	}
	sn := &shortNames{m: cache.New[string, string](512)}
	s.short.Add(did, sn)
	return sn
}

func (sn *shortNames) add(handle string) {
	if handle == "" || handle == "handle.invalid" {
		return
	}
	first, _, _ := strings.Cut(handle, ".")
	sn.m.Add(strings.ToLower(first), handle)
}

func (sn *shortNames) get(name string) (string, bool) {
	return sn.m.Get(strings.ToLower(name))
}

// hourCount tracks per-viewer request counts for plausible rate headers.
type hourCount struct {
	mu    sync.Mutex
	hour  int64
	count int
}

func (s *Server) countRequest(did string) (used int, reset time.Time) {
	now := s.now()
	h := now.Unix() / 3600
	hc, ok := s.usage.Get(did)
	if !ok {
		hc = &hourCount{}
		s.usage.Add(did, hc)
	}
	hc.mu.Lock()
	defer hc.mu.Unlock()
	if hc.hour != h {
		hc.hour, hc.count = h, 0
	}
	hc.count++
	return hc.count, time.Unix((h+1)*3600, 0)
}

// generation returns the viewer's response-cache generation; writes bump it
// so a client sees its own post immediately.
func (s *Server) generation(did string) int {
	g, _ := s.gens.Get(did)
	return g
}

func (s *Server) bumpGeneration(did string) {
	s.genMu.Lock()
	defer s.genMu.Unlock()
	g, _ := s.gens.Get(did)
	s.gens.Add(did, g+1)
}

// publicCaller calls the public AppView without credentials.
type publicCaller struct{ c *atp.Client }

func (p publicCaller) Do(ctx context.Context, r *atp.Request, out any) error {
	if !strings.HasPrefix(r.NSID, "app.bsky.") {
		return errors.New("api: authentication required")
	}
	return p.c.Do(ctx, r, out)
}

func queryValues(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			v.Set(kv[i], kv[i+1])
		}
	}
	return v
}
