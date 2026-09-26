package api

import (
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/j4ckxyz/mockingbird/internal/cache"
	"github.com/j4ckxyz/mockingbird/internal/secret"
	"github.com/j4ckxyz/mockingbird/internal/session"
)

// OAuth model
//
// Twitter clients ship their own consumer key and secret, which the bridge
// cannot know. So the access token the bridge issues (256 random bits,
// prefixed "<user_id>-" like Twitter's) is treated as a bearer credential:
// possession is authentication, and signatures from unknown consumers are
// not checked. When an operator configures a consumer's secret
// (MB_OAUTH_CONSUMERS), requests from that consumer must carry a valid
// HMAC-SHA1 or PLAINTEXT signature, a timestamp inside the window and an
// unused nonce. Only an HMAC of the token is stored server-side.

type requestToken struct {
	secret    string
	consumer  string
	callback  string
	verifier  string
	login     *session.LoginResult
	createdAt time.Time
}

type oauthStore struct {
	tokens *cache.LRU[string, *requestToken]
	nonces *cache.LRU[string, bool]
	ttl    time.Duration
}

func newOAuthStore(ttl time.Duration) *oauthStore {
	return &oauthStore{tokens: cache.New[string, *requestToken](20_000), nonces: cache.New[string, bool](200_000), ttl: ttl}
}

// parseOAuthHeader parses `OAuth k="v", k2="v2"`.
func parseOAuthHeader(h string) map[string]string {
	out := map[string]string{}
	h = strings.TrimSpace(h[len("OAuth "):])
	for _, part := range strings.Split(h, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		if dv, err := url.QueryUnescape(strings.ReplaceAll(v, "+", "%2B")); err == nil {
			v = dv
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

// oauthParams merges OAuth protocol parameters from the header and form.
func oauthParams(c *Ctx) map[string]string {
	p := map[string]string{}
	if h := c.r.Header.Get("Authorization"); len(h) > 6 && strings.EqualFold(h[:6], "oauth ") {
		p = parseOAuthHeader(h)
	}
	for k, v := range c.Form {
		if strings.HasPrefix(k, "oauth_") && len(v) > 0 {
			if _, ok := p[k]; !ok {
				p[k] = v[0]
			}
		}
	}
	return p
}

// percentEncode is RFC 5849 section 3.6 encoding.
func percentEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '.' || ch == '_' || ch == '~' {
			b.WriteByte(ch)
		} else {
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

// signatureBase builds the RFC 5849 signature base string.
func signatureBase(method, baseURL string, params [][2]string) string {
	enc := make([]string, 0, len(params))
	for _, kv := range params {
		enc = append(enc, percentEncode(kv[0])+"="+percentEncode(kv[1]))
	}
	sort.Strings(enc)
	return strings.ToUpper(method) + "&" + percentEncode(baseURL) + "&" + percentEncode(strings.Join(enc, "&"))
}

// hmacSHA1 signs a base string.
func hmacSHA1(base, consumerSecret, tokenSecret string) string {
	m := hmac.New(sha1.New, []byte(percentEncode(consumerSecret)+"&"+percentEncode(tokenSecret)))
	m.Write([]byte(base))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// requestBaseURL reconstructs the URL the client signed.
func (s *Server) requestBaseURL(r *http.Request) string {
	scheme := s.scheme(r)
	host := strings.ToLower(r.Host)
	if h, port, err := splitPort(host); err == nil && ((scheme == "http" && port == "80") || (scheme == "https" && port == "443")) {
		host = h
	}
	return scheme + "://" + host + r.URL.Path
}

func splitPort(h string) (string, string, error) {
	i := strings.LastIndexByte(h, ':')
	if i < 0 || strings.Contains(h[i:], "]") {
		return h, "", errors.New("no port")
	}
	return h[:i], h[i+1:], nil
}

// verifySignature checks a request from a configured consumer.
func (s *Server) verifySignature(c *Ctx, p map[string]string, consumerSecret, tokenSecret string) error {
	sig := p["oauth_signature"]
	if sig == "" {
		return errors.New("missing signature")
	}
	if w := s.cfg.OAuthTimestampWindow; w > 0 {
		ts, err := strconv.ParseInt(p["oauth_timestamp"], 10, 64)
		if err != nil || s.now().Sub(time.Unix(ts, 0)).Abs() > w {
			return errors.New("timestamp outside window")
		}
		nonce := p["oauth_consumer_key"] + "|" + p["oauth_timestamp"] + "|" + p["oauth_nonce"]
		if _, used := s.oauth.nonces.Get(nonce); used {
			return errors.New("nonce reused")
		}
		s.oauth.nonces.AddTTL(nonce, true, 2*w)
	}
	switch strings.ToUpper(p["oauth_signature_method"]) {
	case "PLAINTEXT":
		want := percentEncode(consumerSecret) + "&" + percentEncode(tokenSecret)
		if !secret.EqualString(sig, want) {
			return errors.New("bad signature")
		}
		return nil
	case "HMAC-SHA1", "":
	default:
		return errors.New("unsupported signature method")
	}
	var params [][2]string
	for k, v := range p {
		if k != "oauth_signature" && k != "realm" {
			params = append(params, [2]string{k, v})
		}
	}
	for k, vs := range c.r.URL.Query() {
		if _, dup := p[k]; dup && strings.HasPrefix(k, "oauth_") {
			continue
		}
		for _, v := range vs {
			params = append(params, [2]string{k, v})
		}
	}
	if c.r.PostForm != nil && strings.HasPrefix(c.r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		for k, vs := range c.r.PostForm {
			if _, dup := p[k]; dup && strings.HasPrefix(k, "oauth_") {
				continue
			}
			for _, v := range vs {
				params = append(params, [2]string{k, v})
			}
		}
	}
	want := hmacSHA1(signatureBase(c.r.Method, s.requestBaseURL(c.r), params), consumerSecret, tokenSecret)
	if !secret.EqualString(sig, want) {
		return errors.New("bad signature")
	}
	return nil
}

// oauthSession authenticates an API request carrying an access token.
func (s *Server) oauthSession(c *Ctx, p map[string]string) (*session.Session, error) {
	sess, err := s.d.Sessions.OAuth(c.Context(), p["oauth_token"])
	if err != nil {
		return nil, err
	}
	if cs, ok := s.cfg.OAuthConsumers[sess.ConsumerKey]; ok {
		if p["oauth_consumer_key"] != sess.ConsumerKey {
			return nil, session.ErrRevoked
		}
		if err := s.verifySignature(c, p, cs, sess.TokenSecret); err != nil {
			return nil, &APIError{Status: 401, Msg: "Invalid / expired Token", Cause: err}
		}
	}
	return sess, nil
}

func formResp(v url.Values) *Resp {
	// Twitter's token endpoints answered with a bare form-encoded body.
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	order := map[string]int{"oauth_token": 0, "oauth_token_secret": 1, "oauth_callback_confirmed": 2, "user_id": 3, "screen_name": 4}
	sort.Slice(keys, func(i, j int) bool { return order[keys[i]] < order[keys[j]] })
	var parts []string
	for _, k := range keys {
		parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v.Get(k)))
	}
	return &Resp{Raw: []byte(strings.Join(parts, "&")), ContentType: "text/html; charset=utf-8"}
}

func (s *Server) oauthRequestToken(c *Ctx) (*Resp, error) {
	p := oauthParams(c)
	consumer := p["oauth_consumer_key"]
	if cs, ok := s.cfg.OAuthConsumers[consumer]; ok {
		if err := s.verifySignature(c, p, cs, ""); err != nil {
			return nil, &APIError{Status: 401, Msg: "Failed to validate oauth signature and token", Cause: err}
		}
	}
	tok, sec := secret.Token(24), secret.Token(32)
	cb := p["oauth_callback"]
	if cb == "" {
		cb = "oob"
	}
	s.oauth.tokens.AddTTL(tok, &requestToken{secret: sec, consumer: consumer, callback: cb, createdAt: s.now()}, s.oauth.ttl)
	return formResp(url.Values{"oauth_token": {tok}, "oauth_token_secret": {sec}, "oauth_callback_confirmed": {"true"}}), nil
}

func (s *Server) oauthAccessToken(c *Ctx) (*Resp, error) {
	p := oauthParams(c)
	consumer := p["oauth_consumer_key"]
	cs, known := s.cfg.OAuthConsumers[consumer]

	var login *session.LoginResult
	if c.Form.Get("x_auth_mode") == "client_auth" || c.Form.Get("x_auth_username") != "" {
		// xAuth: credentials exchanged directly for an access token.
		if known {
			if err := s.verifySignature(c, p, cs, ""); err != nil {
				return nil, &APIError{Status: 401, Msg: "Failed to validate oauth signature and token", Cause: err}
			}
		}
		res, err := s.d.Sessions.Login(c.Context(), c.IP, c.Form.Get("x_auth_username"), c.Form.Get("x_auth_password"))
		if err != nil {
			if s.d.Metrics != nil {
				s.d.Metrics.Login(loginOutcome(err))
			}
			return nil, authError(err)
		}
		login = res
	} else {
		rt, ok := s.oauth.tokens.Get(p["oauth_token"])
		if !ok || rt.login == nil || rt.consumer != consumer {
			return nil, &APIError{Status: 401, Msg: "Invalid / expired Token"}
		}
		if known {
			if err := s.verifySignature(c, p, cs, rt.secret); err != nil {
				return nil, &APIError{Status: 401, Msg: "Failed to validate oauth signature and token", Cause: err}
			}
		}
		if rt.verifier != "" && !secret.EqualString(p["oauth_verifier"], rt.verifier) {
			return nil, &APIError{Status: 401, Msg: "Invalid oauth_verifier parameter"}
		}
		s.oauth.tokens.Remove(p["oauth_token"])
		login = rt.login
	}
	uid, err := s.builder(c).UserID(c.Context(), login.Identity.DID, login.Identity.Handle)
	if err != nil {
		return nil, err
	}
	tok, sec, _, err := s.d.Sessions.NewOAuthSession(c.Context(), login, consumer, uid)
	if err != nil {
		return nil, err
	}
	return formResp(url.Values{"oauth_token": {tok}, "oauth_token_secret": {sec},
		"user_id": {strconv.FormatInt(uid, 10)}, "screen_name": {login.Identity.Handle}}), nil
}

// AuthorizePage is the data for the OAuth login form.
type AuthorizePage struct {
	Token    string
	Error    string
	Handle   string
	Consumer string
	PIN      string
}

// OAuthAuthorize handles GET/POST /oauth/authorize (and /oauth/authenticate):
// a plain form that works in iPhone OS 3 Mobile Safari without JavaScript.
func (s *Server) OAuthAuthorize(w http.ResponseWriter, r *http.Request, render func(http.ResponseWriter, *http.Request, AuthorizePage)) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tok := r.Form.Get("oauth_token")
	rt, ok := s.oauth.tokens.Get(tok)
	if !ok {
		render(w, r, AuthorizePage{Error: "This sign-in link has expired. Go back to your app and try again."})
		return
	}
	page := AuthorizePage{Token: tok, Consumer: rt.consumer}
	if r.Method != http.MethodPost {
		render(w, r, page)
		return
	}
	page.Handle = r.PostForm.Get("handle")
	res, err := s.d.Sessions.Login(r.Context(), s.clientIP(r), page.Handle, r.PostForm.Get("password"))
	if err != nil {
		page.Error = html.UnescapeString(authError(err).(*APIError).Msg)
		if page.Error == "Could not authenticate you." {
			page.Error = "That handle and app password did not work."
		}
		render(w, r, page)
		return
	}
	rt.login = res
	n, err := crand.Int(crand.Reader, big.NewInt(9_000_000))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rt.verifier = strconv.FormatInt(n.Int64()+1_000_000, 10) // 7-digit PIN, as Twitter used
	if rt.callback == "oob" || rt.callback == "" {
		page.PIN = rt.verifier
		render(w, r, page)
		return
	}
	u, err := url.Parse(rt.callback)
	if err != nil || (u.Scheme == "javascript" || u.Scheme == "data") {
		page.Error = "The app supplied an invalid callback."
		render(w, r, page)
		return
	}
	q := u.Query()
	q.Set("oauth_token", tok)
	q.Set("oauth_verifier", rt.verifier)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}
