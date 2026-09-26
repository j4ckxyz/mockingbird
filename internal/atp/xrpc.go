// Package atp is a small XRPC client and the subset of Bluesky lexicon types
// the bridge reads.
//
// The types are deliberately lenient hand-written structs rather than
// indigo's generated ones: indigo's record decoder fails the whole response on
// any unregistered $type, and one exotic quote-embed must not blank a user's
// timeline. Unknown fields and union members are simply ignored here.
package atp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// Error is an XRPC error response.
type Error struct {
	Status  int    `json:"-"`
	Name    string `json:"error"`
	Message string `json:"message"`
	NSID    string `json:"-"`
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("xrpc %s: %d %s: %s", e.NSID, e.Status, e.Name, e.Message)
	}
	return fmt.Sprintf("xrpc %s: %d %s", e.NSID, e.Status, e.Name)
}

// IsError reports whether err is an XRPC error with one of the given names
// (or, when names is empty, any XRPC error).
func IsError(err error, names ...string) bool {
	var xe *Error
	if !errors.As(err, &xe) {
		return false
	}
	if len(names) == 0 {
		return true
	}
	for _, n := range names {
		if xe.Name == n {
			return true
		}
	}
	return false
}

// Status returns the HTTP status of an XRPC error, or 0.
func Status(err error) int {
	var xe *Error
	if errors.As(err, &xe) {
		return xe.Status
	}
	return 0
}

// CallHook observes every upstream call (for metrics).
type CallHook func(nsid string, status int, d time.Duration)

// Client talks XRPC to one host.
type Client struct {
	HTTP *http.Client
	Host string // e.g. https://pds.example.com
	Hook CallHook
}

// Request describes one XRPC call.
type Request struct {
	Method      string // GET or POST
	NSID        string
	Params      url.Values
	Body        any    // JSON-encoded unless it is an io.Reader
	ContentType string // for io.Reader bodies
	Token       string // bearer token
	Proxy       string // atproto-proxy header value
}

// Do performs the call and decodes a JSON response into out (if non-nil).
func (c *Client) Do(ctx context.Context, r *Request, out any) error {
	u := c.Host + "/xrpc/" + r.NSID
	if len(r.Params) > 0 {
		u += "?" + r.Params.Encode()
	}
	var body io.Reader
	ct := r.ContentType
	switch b := r.Body.(type) {
	case nil:
	case io.Reader:
		body = b
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
		ct = "application/json"
	}
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	req.Header.Set("Accept", "application/json")
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	if r.Proxy != "" {
		req.Header.Set("atproto-proxy", r.Proxy)
	}
	start := time.Now()
	resp, err := c.HTTP.Do(req)
	d := time.Since(start)
	addUpstream(ctx, d)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	if c.Hook != nil {
		c.Hook(r.NSID, status, d)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		xe := &Error{Status: resp.StatusCode, NSID: r.NSID}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		_ = json.Unmarshal(b, xe)
		if xe.Name == "" {
			xe.Name = http.StatusText(resp.StatusCode)
		}
		return xe
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if w, ok := out.(io.Writer); ok {
		_, err = io.Copy(w, resp.Body)
		return err
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return fmt.Errorf("xrpc %s: unexpected content type %q", r.NSID, resp.Header.Get("Content-Type"))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type upstreamKey struct{}

// WithUpstreamTimer attaches a counter that accumulates time spent waiting on
// upstream calls, so request metrics can report translation overhead alone.
func WithUpstreamTimer(ctx context.Context) (context.Context, *atomic.Int64) {
	var n atomic.Int64
	return context.WithValue(ctx, upstreamKey{}, &n), &n
}

func addUpstream(ctx context.Context, d time.Duration) {
	if n, ok := ctx.Value(upstreamKey{}).(*atomic.Int64); ok {
		n.Add(int64(d))
	}
}

// AddUpstream lets other outbound fetches (images, identity) count as upstream.
func AddUpstream(ctx context.Context, d time.Duration) { addUpstream(ctx, d) }
