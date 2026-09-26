package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackgilbert/mockingbird/internal/atp"
	"github.com/jackgilbert/mockingbird/internal/session"
	"github.com/jackgilbert/mockingbird/internal/twitter"
)

// Ctx is one API request.
type Ctx struct {
	s      *Server
	w      http.ResponseWriter
	r      *http.Request
	IP     string
	Path   string // normalised, no version prefix or extension
	Format string // json, xml, rss, atom
	Params map[string]string
	Form   url.Values
	Sess   *session.Session
	route  *route
}

// Context returns the request context.
func (c *Ctx) Context() context.Context { return c.r.Context() }

// Caller returns the session, or the public AppView when anonymous.
func (c *Ctx) Caller() caller {
	if c.Sess != nil {
		return c.Sess
	}
	return publicCaller{c.s.d.Public}
}

type caller interface {
	Do(ctx context.Context, r *atp.Request, out any) error
}

// Do is a shortcut for c.Caller().Do.
func (c *Ctx) Do(r *atp.Request, out any) error { return c.Caller().Do(c.Context(), r, out) }

// Get performs an XRPC query.
func (c *Ctx) Get(nsid string, params url.Values, out any) error {
	return c.Do(&atp.Request{NSID: nsid, Params: params}, out)
}

// Post performs an XRPC procedure.
func (c *Ctx) Post(nsid string, body any, out any) error {
	return c.Do(&atp.Request{Method: http.MethodPost, NSID: nsid, Body: body}, out)
}

// Arg returns a path parameter, falling back to the form.
func (c *Ctx) Arg(names ...string) string {
	for _, n := range names {
		if v := c.Params[n]; v != "" {
			return v
		}
		if v := strings.TrimSpace(c.Form.Get(n)); v != "" {
			return v
		}
	}
	return ""
}

// IntArg parses an integer argument.
func (c *Ctx) IntArg(name string, def int) int {
	v := c.Arg(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// Int64Arg parses an int64 argument (0 if absent or invalid).
func (c *Ctx) Int64Arg(names ...string) int64 {
	n, _ := strconv.ParseInt(c.Arg(names...), 10, 64)
	return n
}

// Count returns count (or rpp), clamped.
func (c *Ctx) Count(def, max int) int {
	n := c.IntArg("count", 0)
	if n == 0 {
		n = c.IntArg("rpp", 0)
	}
	if n == 0 {
		n = c.IntArg("per_page", def)
	}
	if n < 1 {
		n = def
	}
	if n > max {
		n = max
	}
	return n
}

// Page returns the 1-based page.
func (c *Ctx) Page() int {
	p := c.IntArg("page", 1)
	if p < 1 {
		p = 1
	}
	if p > 50 {
		p = 50
	}
	return p
}

// Resp is a handler's successful result.
type Resp struct {
	// Value is encoded as JSON, or as XML under Root.
	Value any
	Root  string
	// Item, when set, encodes Value (a slice) as <Root type="array"><Item/>.
	Item string
	// Statuses enables RSS/Atom output.
	Statuses []twitter.Status
	Feed     *twitter.FeedInfo
	// Raw bypasses encoding.
	Raw         []byte
	ContentType string
	Status      int
	// Cacheable marks GET responses that may be served from the response cache.
	Cacheable bool
}

// APIError is a Twitter-style error.
type APIError struct {
	Status int
	Msg    string
	Cause  error
}

func (e *APIError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%d %s: %v", e.Status, e.Msg, e.Cause)
	}
	return fmt.Sprintf("%d %s", e.Status, e.Msg)
}

func (e *APIError) Unwrap() error { return e.Cause }

// Error constructors with Twitter's period wording.
func errNotFound() error           { return &APIError{Status: 404, Msg: "Not found"} }
func errUnauthorized() error       { return &APIError{Status: 401, Msg: "Could not authenticate you."} }
func errBadRequest(m string) error { return &APIError{Status: 400, Msg: m} }
func errForbidden(m string) error  { return &APIError{Status: 403, Msg: m} }

// upstreamErr maps an XRPC failure to a client error.
func upstreamErr(err error) error {
	if err == nil {
		return nil
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return err
	}
	switch {
	case errors.Is(err, session.ErrRevoked):
		return &APIError{Status: 401, Msg: "Could not authenticate you. Your Bluesky session has expired or the app password was revoked.", Cause: err}
	case errors.Is(err, context.DeadlineExceeded):
		return &APIError{Status: 502, Msg: "Bluesky is taking too long to respond. Try again.", Cause: err}
	}
	switch atp.Status(err) {
	case 0:
		return &APIError{Status: 502, Msg: "Bluesky is over capacity.", Cause: err}
	case 400:
		if atp.IsError(err, "InvalidToken") && strings.Contains(err.Error(), "scope") {
			return &APIError{Status: 403, Msg: "This app password does not allow direct messages.", Cause: err}
		}
		if atp.IsError(err, "NotFound", "RecordNotFound", "InvalidRequest") {
			return &APIError{Status: 404, Msg: "Not found", Cause: err}
		}
		return &APIError{Status: 400, Msg: "Bluesky rejected the request.", Cause: err}
	case 401:
		return &APIError{Status: 401, Msg: "Could not authenticate you.", Cause: err}
	case 404:
		return &APIError{Status: 404, Msg: "Not found", Cause: err}
	case 429:
		return &APIError{Status: 400, Msg: "Rate limit exceeded. Bluesky is limiting this account; try again shortly.", Cause: err}
	}
	return &APIError{Status: 502, Msg: "Bluesky is over capacity.", Cause: err}
}

// render writes a successful response in the requested format.
func (c *Ctx) render(res *Resp) {
	status := res.Status
	if status == 0 {
		status = 200
	}
	body, ct, err := c.encode(res)
	if err != nil {
		var ae *APIError
		if !errors.As(err, &ae) {
			ae = &APIError{Status: 500, Msg: "Something is technically wrong.", Cause: err}
		}
		c.Form.Del("callback") // never echo a rejected callback
		c.renderError(ae)
		return
	}
	if c.Sess != nil && res.Cacheable && c.r.Method == http.MethodGet && status == 200 {
		c.s.resp.AddTTL(c.cacheKey(), cachedResponse{body: body, contentType: ct, status: status}, c.s.cfg.ResponseCacheTTL)
	}
	c.send(status, ct, body)
}

func (c *Ctx) encode(res *Resp) ([]byte, string, error) {
	if res.Raw != nil {
		return res.Raw, res.ContentType, nil
	}
	switch c.Format {
	case "xml":
		if res.Item != "" {
			return twitter.EncodeXMLArray(res.Root, res.Item, res.Value), "application/xml; charset=utf-8", nil
		}
		return twitter.EncodeXML(res.Root, res.Value), "application/xml; charset=utf-8", nil
	case "rss", "atom":
		if res.Feed == nil {
			return nil, "", errors.New("no feed")
		}
		if c.Format == "rss" {
			return twitter.EncodeRSS(*res.Feed, res.Statuses), "application/rss+xml; charset=utf-8", nil
		}
		return twitter.EncodeAtom(*res.Feed, res.Statuses), "application/atom+xml; charset=utf-8", nil
	}
	b, err := twitter.EncodeJSON(res.Value)
	if err != nil {
		return nil, "", err
	}
	if cb := c.Form.Get("callback"); cb != "" {
		if !twitter.ValidCallback(cb) {
			return nil, "", &APIError{Status: 400, Msg: "Invalid callback"}
		}
		return twitter.WrapJSONP(cb, b), "text/javascript; charset=utf-8", nil
	}
	return b, "application/json; charset=utf-8", nil
}

// send writes status, headers and body, honouring suppress_response_codes.
func (c *Ctx) send(status int, ct string, body []byte) {
	if c.Sess != nil {
		c.rateHeaders()
	}
	if status >= 400 && c.Form.Get("suppress_response_codes") != "" {
		status = 200
	}
	h := c.w.Header()
	h.Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	h.Set("Pragma", "no-cache")
	write(c.w, c.r, status, ct, body)
}

func (c *Ctx) rateHeaders() {
	used, reset := c.s.countRequest(c.Sess.DID)
	limit := c.s.cfg.ReportedRateLimit
	// Report generous, stable headroom so clients never back off on their own.
	remaining := limit - min(used, limit/10)
	h := c.w.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	h.Set("X-RateLimit-Class", "api")
}

// renderError writes a Twitter-style error.
func (c *Ctx) renderError(err error) {
	var ae *APIError
	if !errors.As(err, &ae) {
		ae = upstreamErr(err).(*APIError)
	}
	if ae.Status >= 500 || ae.Cause != nil {
		c.s.log.Warn("request failed", "path", c.r.URL.Path, "status", ae.Status, "err", ae)
	}
	if ae.Status == 401 {
		c.w.Header().Set("WWW-Authenticate", `Basic realm="Twitter API"`)
	}
	reqPath := c.r.URL.Path
	body := twitter.ErrorBody{Request: reqPath, Error: ae.Msg}
	var b []byte
	var ct string
	switch c.Format {
	case "xml", "rss", "atom":
		var w twitter.XMLWriter
		w.Header()
		w.Open("hash")
		w.Elem("request", body.Request)
		w.Elem("error", body.Error)
		w.Close("hash")
		b, ct = w.Bytes(), "application/xml; charset=utf-8"
	default:
		b, _ = twitter.EncodeJSON(body)
		ct = "application/json; charset=utf-8"
		if cb := c.Form.Get("callback"); cb != "" && twitter.ValidCallback(cb) {
			b, ct = twitter.WrapJSONP(cb, b), "text/javascript; charset=utf-8"
		}
	}
	c.send(ae.Status, ct, b)
}

func (c *Ctx) cacheKey() string {
	q := c.Form.Encode() // sorted by key
	return fmt.Sprintf("%x|%d|%s|%s|%s", c.Sess.Key, c.s.generation(c.Sess.DID), c.Path, c.Format, q)
}

// window is the since/max range of a timeline request.
type window struct {
	since    time.Time
	sinceKey string
	max      time.Time
	maxKey   string
	cursor   string // cached upstream cursor to resume from
}

func (c *Ctx) hintKey(feed string, id int64) string {
	did := ""
	if c.Sess != nil {
		did = c.Sess.DID
	}
	return did + "|" + feed + "|" + strconv.FormatInt(id, 10)
}

// window resolves since_id and max_id to sort times. Bridge IDs are not
// time-ordered across feeds, so "max_id = oldest - 1" (which clients send
// when paging back) cannot be taken literally: if the ID just above max_id is
// the last item we served on this feed, that is what the client meant.
func (c *Ctx) window(feed string) window {
	var w window
	ctx := c.Context()
	if id := c.Int64Arg("since_id"); id > 0 {
		if uri, t, err := c.s.d.Store.StatusByID(ctx, id); err == nil && !t.IsZero() {
			w.since, w.sinceKey = t, uri
		}
	}
	id := c.Int64Arg("max_id")
	if id <= 0 {
		return w
	}
	if cur, ok := c.s.cursors.Get(c.hintKey(feed, id+1)); ok {
		if _, t, err := c.s.d.Store.StatusByID(ctx, id+1); err == nil && !t.IsZero() {
			w.max, w.cursor = t.Add(-time.Millisecond), cur
			return w
		}
	}
	if uri, t, err := c.s.d.Store.StatusByID(ctx, id); err == nil && !t.IsZero() {
		w.max, w.maxKey = t, uri
		if cur, ok := c.s.cursors.Get(c.hintKey(feed, id)); ok {
			w.cursor = cur
		}
	}
	return w
}

// remember records where a served page ended, for the next max_id request.
func (c *Ctx) remember(feed string, lastID int64, cursor string) {
	if lastID > 0 {
		c.s.cursors.Add(c.hintKey(feed, lastID), cursor)
	}
}
