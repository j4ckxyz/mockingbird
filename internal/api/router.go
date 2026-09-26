package api

import (
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sync/singleflight"

	"github.com/j4ckxyz/mockingbird/internal/session"
)

// Auth modes.
const (
	authNone     = iota // no credentials used
	authOptional        // use credentials if present, else anonymous
	authRequired
)

type handler func(c *Ctx) (*Resp, error)

type route struct {
	method  string // GET, POST, or "" for either
	pattern []string
	auth    int
	formats string // allowed formats, e.g. "json xml rss atom"
	h       handler
	name    string
	// params lists the query parameters this endpoint understands; others
	// are logged once so gaps in client support are visible.
	params string
}

type singleflightGroup = singleflight.Group

// commonParams are accepted everywhere.
const commonParams = "callback suppress_response_codes oauth_consumer_key oauth_token oauth_signature oauth_signature_method oauth_timestamp oauth_nonce oauth_version source include_entities include_rts trim_user skip_user include_my_retweet cursor _ lang"

func (s *Server) add(method, pattern string, auth int, formats string, h handler, params string) {
	s.routes = append(s.routes, &route{method: method, pattern: strings.Split(pattern, "/"), auth: auth,
		formats: formats, h: h, name: pattern, params: params})
}

func (r *route) match(parts []string) (map[string]string, bool) {
	if len(parts) != len(r.pattern) {
		return nil, false
	}
	var params map[string]string
	for i, p := range r.pattern {
		if strings.HasPrefix(p, ":") {
			if parts[i] == "" {
				return nil, false
			}
			if params == nil {
				params = map[string]string{}
			}
			params[p[1:]] = parts[i]
			continue
		}
		if p != parts[i] {
			return nil, false
		}
	}
	return params, true
}

// normalize strips the version prefix and format extension:
// "/1/statuses/show/123.json" -> ("statuses/show/123", "json").
func normalize(p string) (string, string) {
	p = strings.TrimPrefix(p, "/")
	for _, pre := range []string{"1/", "api/"} {
		if strings.HasPrefix(p, pre) && !strings.HasPrefix(p, "api/upload") {
			p = strings.TrimPrefix(p, pre)
			break
		}
	}
	p = strings.TrimSuffix(p, "/")
	format := ""
	if i := strings.LastIndexByte(p, '.'); i > strings.LastIndexByte(p, '/') {
		switch ext := strings.ToLower(p[i+1:]); ext {
		case "json", "xml", "rss", "atom":
			format, p = ext, p[:i]
		}
	}
	return p, format
}

// dispatch routes a request and returns the route name for metrics.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, ip string) string {
	host := hostOnly(r.Host)
	path, format := normalize(r.URL.Path)

	// Web pages and assets live outside the API namespace.
	if s.d.Web != nil && format == "" && isWebPath(path) {
		s.d.Web.ServeHTTP(w, r)
		return "web"
	}
	if strings.HasPrefix(r.URL.Path, "/img/") {
		s.d.Images.ServeHTTP(w, r)
		return "img"
	}
	if s.isSearchHost(host) && path == "" {
		http.Redirect(w, r, s.cfg.PublicURL.String()+"/", http.StatusFound)
		return "web"
	}

	parts := strings.Split(path, "/")
	var rt *route
	var params map[string]string
	methodMismatch := false
	for _, cand := range s.routes {
		p, ok := cand.match(parts)
		if !ok {
			continue
		}
		if cand.method != "" && cand.method != r.Method && !(cand.method == "GET" && r.Method == "HEAD") {
			methodMismatch = true
			continue
		}
		rt, params = cand, p
		break
	}

	if err := r.ParseForm(); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return "toolarge"
		}
	}
	if format == "" {
		format = "json"
		if rt != nil && strings.HasPrefix(rt.formats, "xml") {
			format = "xml"
		}
	}
	c := &Ctx{s: s, w: w, r: r, IP: ip, Path: path, Format: format, Params: params, Form: r.Form, route: rt}

	if rt == nil {
		if s.d.Web != nil && format == "json" && !strings.HasSuffix(r.URL.Path, ".json") {
			// Plain web paths such as /alice/status/123 from in-app browsers.
			s.d.Web.ServeHTTP(w, r)
			return "web"
		}
		s.unknown.record(r, path, format)
		if s.d.Metrics != nil {
			s.d.Metrics.Unknown()
		}
		if methodMismatch {
			c.renderError(&APIError{Status: 405, Msg: "This method requires a different HTTP method."})
			return "method"
		}
		c.renderError(errNotFound())
		return "unknown"
	}
	if !strings.Contains(" "+rt.formats+" ", " "+format+" ") {
		c.renderError(errNotFound())
		return rt.name
	}
	s.unknown.params(rt, r.Form)

	if rt.auth != authNone {
		sess, err := s.authenticate(c)
		switch {
		case err == nil:
			c.Sess = sess
		case errors.Is(err, errNoCredentials) && rt.auth == authOptional:
		default:
			c.renderError(authError(err))
			return rt.name
		}
	}
	if c.Sess != nil && !s.acLimit.Allow(c.Sess.DID) {
		c.renderError(&APIError{Status: 400, Msg: "Rate limit exceeded. Clients may not make more than 350 requests per hour."})
		return rt.name
	}
	if c.Sess != nil && r.Method == http.MethodGet {
		if cr, ok := s.resp.Get(c.cacheKey()); ok {
			if s.d.Metrics != nil {
				s.d.Metrics.ResponseCache(true)
			}
			c.sendBody(cr.status, cr.contentType, cr.raw, cr.gz)
			return rt.name
		}
		if s.d.Metrics != nil {
			s.d.Metrics.ResponseCache(false)
		}
	}
	var res *Resp
	var err error
	if c.Sess != nil && r.Method == http.MethodGet {
		// Clients often fire the same request twice (launch plus timer, or
		// two views of one timeline). Identical concurrent GETs from one
		// credential share a single upstream round; each renders its own copy.
		v, e, _ := s.sf.Do("req:"+c.cacheKey(), func() (any, error) { return rt.h(c) })
		if v != nil {
			res = v.(*Resp)
		}
		err = e
	} else {
		res, err = rt.h(c)
	}
	if err != nil {
		c.renderError(err)
		return rt.name
	}
	if r.Method == http.MethodPost && c.Sess != nil {
		s.bumpGeneration(c.Sess.DID)
	}
	c.render(res)
	return rt.name
}

// isWebPath reports paths served by the web handler rather than the API.
func isWebPath(p string) bool {
	switch {
	case p == "", p == "ca.crt", p == "ca.cer", p == "mockingbird.mobileconfig", p == "theme", p == "healthz", p == "robots.txt", p == "favicon.ico":
		return true
	case strings.HasPrefix(p, "p/"), strings.HasPrefix(p, "static/"), strings.HasPrefix(p, "m/"),
		p == "oauth/authorize", p == "oauth/authenticate", p == "setup" || strings.HasPrefix(p, "setup/"):
		return true
	}
	return false
}

var errNoCredentials = errors.New("api: no credentials")

// authenticate extracts Basic or OAuth credentials.
func (s *Server) authenticate(c *Ctx) (*session.Session, error) {
	r := c.r
	authz := r.Header.Get("Authorization")
	switch {
	case len(authz) > 6 && strings.EqualFold(authz[:6], "basic "):
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(authz[6:]))
		if err != nil {
			return nil, session.ErrBadCredentials
		}
		user, pass, ok := strings.Cut(string(raw), ":")
		if !ok {
			return nil, session.ErrBadCredentials
		}
		sess, err := s.d.Sessions.Basic(r.Context(), c.IP, user, pass)
		if s.d.Metrics != nil && err != nil {
			s.d.Metrics.Login(loginOutcome(err))
		}
		return sess, err
	case len(authz) > 6 && strings.EqualFold(authz[:6], "oauth "):
		p := parseOAuthHeader(authz)
		return s.oauthSession(c, p)
	case c.Form.Get("oauth_token") != "":
		p := map[string]string{}
		for k, v := range c.Form {
			if strings.HasPrefix(k, "oauth_") && len(v) > 0 {
				p[k] = v[0]
			}
		}
		return s.oauthSession(c, p)
	}
	return nil, errNoCredentials
}

func loginOutcome(err error) string {
	switch {
	case errors.Is(err, session.ErrBadCredentials), errors.Is(err, session.ErrUnknownAccount):
		return "bad_credentials"
	case errors.Is(err, session.ErrFullPassword):
		return "full_password"
	case errors.Is(err, session.ErrLocked):
		return "locked"
	}
	return "error"
}

func authError(err error) error {
	switch {
	case errors.Is(err, errNoCredentials), errors.Is(err, session.ErrBadCredentials), errors.Is(err, session.ErrUnknownAccount):
		return errUnauthorized()
	case errors.Is(err, session.ErrFullPassword):
		return &APIError{Status: 401, Msg: "Use a Bluesky app password, not your account password (Settings > Privacy and security > App passwords)."}
	case errors.Is(err, session.ErrLocked):
		return &APIError{Status: 401, Msg: "Too many failed logins. Wait a few minutes and try again."}
	case errors.Is(err, session.ErrRevoked):
		return &APIError{Status: 401, Msg: "Could not authenticate you. Your authorization has expired or was revoked."}
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae
	}
	return upstreamErr(err)
}

// unknownLog records endpoints and parameters the bridge does not handle,
// with counts, and logs each distinct one once.
type unknownLog struct {
	mu     sync.Mutex
	counts map[string]int
	log    *slog.Logger
}

func newUnknownLog(l *slog.Logger) *unknownLog {
	return &unknownLog{counts: map[string]int{}, log: l}
}

func (u *unknownLog) bump(key string, attrs ...any) {
	u.mu.Lock()
	n := u.counts[key]
	if n == 0 && len(u.counts) >= 5000 {
		u.mu.Unlock()
		return
	}
	u.counts[key] = n + 1
	u.mu.Unlock()
	if n == 0 {
		u.log.Warn("unimplemented", append([]any{"what", key}, attrs...)...)
	}
}

// record logs an unknown endpoint: method, templated path, parameter names
// (never values) and the client's User-Agent.
func (u *unknownLog) record(r *http.Request, path, format string) {
	names := make([]string, 0, len(r.Form))
	for k := range r.Form {
		names = append(names, k)
	}
	sort.Strings(names)
	key := "endpoint " + r.Method + " /" + templatePath(path) + "." + format
	u.bump(key, "host", hostOnly(r.Host), "params", strings.Join(names, ","), "user_agent", r.UserAgent())
}

// params logs query parameters a known endpoint ignores.
func (u *unknownLog) params(rt *route, form url.Values) {
	for k := range form {
		if strings.Contains(" "+rt.params+" "+commonParams+" ", " "+k+" ") {
			continue
		}
		u.bump("param "+rt.name+"?"+k, "endpoint", rt.name)
	}
}

// Snapshot returns the counts.
func (u *unknownLog) Snapshot() map[string]int {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make(map[string]int, len(u.counts))
	for k, v := range u.counts {
		out[k] = v
	}
	return out
}

// templatePath replaces numeric and user-looking segments so logs group.
func templatePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		if s == "" {
			continue
		}
		allDigits := true
		for _, r := range s {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits || (i > 1 && strings.Contains(s, ".")) {
			parts[i] = ":id"
		}
	}
	return strings.Join(parts, "/")
}
