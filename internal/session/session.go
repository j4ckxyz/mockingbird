// Package session turns Twitter-client credentials into cached Bluesky
// sessions.
//
// createSession is heavily rate limited upstream, so it runs only on a cache
// miss. Basic Auth sessions are keyed by HMAC(server secret, identifier,
// password): only the right password maps to the cached session, and the
// password itself is never stored or logged. Refresh tokens are sealed with
// AES-GCM before they touch disk.
package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/jackgilbert/mockingbird/internal/atp"
	"github.com/jackgilbert/mockingbird/internal/cache"
	"github.com/jackgilbert/mockingbird/internal/ident"
	"github.com/jackgilbert/mockingbird/internal/ratelimit"
	"github.com/jackgilbert/mockingbird/internal/secret"
	"github.com/jackgilbert/mockingbird/internal/store"
)

// Errors surfaced to clients.
var (
	ErrBadCredentials = errors.New("session: invalid handle or app password")
	ErrFullPassword   = errors.New("session: account password used; only app passwords are accepted")
	ErrLocked         = errors.New("session: too many failed logins, try again later")
	ErrRevoked        = errors.New("session: session expired or revoked")
	ErrUnknownAccount = errors.New("session: unknown account")
)

// App password scopes. A full account password gets com.atproto.access.
const (
	ScopeAppPass           = "com.atproto.appPass"
	ScopeAppPassPrivileged = "com.atproto.appPassPrivileged"
	ScopeFullAccess        = "com.atproto.access"
)

// Kinds of session.
const (
	KindBasic = "basic"
	KindOAuth = "oauth"
)

// Session is a live Bluesky session bound to one bridge credential.
type Session struct {
	Key         []byte
	Kind        string
	DID         string
	PDS         string
	Scope       string
	ConsumerKey string
	TokenSecret string // OAuth token secret (for known-consumer signature checks)

	mgr *Manager

	mu        sync.Mutex
	handle    string
	access    string
	accessExp time.Time
	refresh   string
	touched   time.Time
}

// Handle returns the account's current handle.
func (s *Session) Handle() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handle
}

// CanDM reports whether the app password grants chat access.
func (s *Session) CanDM() bool { return s.Scope == ScopeAppPassPrivileged }

// Options configure the manager.
type Options struct {
	Store             *store.Store
	Keys              *secret.Keys
	Resolver          ident.Resolver
	HTTP              *http.Client // guarded client for PDS calls
	Hook              atp.CallHook
	AppViewProxy      string
	ChatProxy         string
	DefaultHandleHost string
	Logger            *slog.Logger
	CacheSize         int
}

// Manager owns all sessions.
type Manager struct {
	o        Options
	live     *cache.LRU[string, *Session]
	sf       singleflight.Group
	ipFails  *ratelimit.Failures
	idFails  *ratelimit.Failures
	clientMu sync.Mutex
	clients  map[string]*atp.Client
	now      func() time.Time
}

// NewManager builds a session manager.
func NewManager(o Options) *Manager {
	if o.CacheSize == 0 {
		o.CacheSize = 50_000
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Manager{
		o:    o,
		live: cache.New[string, *Session](o.CacheSize),
		// Per IP: 10 failures, then 1 min lockout doubling to 1 h.
		ipFails: ratelimit.NewFailures(10, 15*time.Minute, time.Minute, time.Hour, 100_000),
		// Per account: 5 failures, then 2 min doubling to 1 h.
		idFails: ratelimit.NewFailures(5, time.Hour, 2*time.Minute, time.Hour, 100_000),
		clients: map[string]*atp.Client{},
		now:     time.Now,
	}
}

// Client returns the shared XRPC client for a PDS host. Connections are
// pooled per host by the underlying transport.
func (m *Manager) Client(host string) *atp.Client {
	m.clientMu.Lock()
	defer m.clientMu.Unlock()
	c, ok := m.clients[host]
	if !ok {
		c = &atp.Client{HTTP: m.o.HTTP, Host: host, Hook: m.o.Hook}
		m.clients[host] = c
	}
	return c
}

// BasicKey derives the session key for a Basic Auth credential.
func (m *Manager) BasicKey(identifier, password string) []byte {
	id := ident.NormalizeIdentifier(identifier, m.o.DefaultHandleHost)
	return m.o.Keys.MACString("basic-session", id, password)
}

// OAuthKey derives the session key for an OAuth access token.
func (m *Manager) OAuthKey(token string) []byte {
	return m.o.Keys.MACString("oauth-session", token)
}

// Basic returns the session for a Basic Auth credential, logging in on a miss.
func (m *Manager) Basic(ctx context.Context, ip, identifier, password string) (*Session, error) {
	if identifier == "" || password == "" {
		return nil, ErrBadCredentials
	}
	key := m.BasicKey(identifier, password)
	if s, err := m.load(ctx, key); err == nil {
		// Refresh now if needed. If the session was revoked upstream we still
		// hold the password for this request, so log in again transparently.
		if _, err := s.token(ctx, false); err == nil {
			return s, nil
		} else if !errors.Is(err, ErrRevoked) {
			return nil, err
		}
	} else if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, ErrRevoked) {
		return nil, err
	}
	v, err, _ := m.sf.Do("login:"+string(key), func() (any, error) {
		if s, err := m.load(ctx, key); err == nil {
			return s, nil
		}
		res, err := m.Login(ctx, ip, identifier, password)
		if err != nil {
			return nil, err
		}
		return m.persist(ctx, key, KindBasic, res, "", "")
	})
	if err != nil {
		return nil, err
	}
	return v.(*Session), nil
}

// OAuth returns the session for an OAuth access token.
func (m *Manager) OAuth(ctx context.Context, token string) (*Session, error) {
	if token == "" {
		return nil, ErrRevoked
	}
	s, err := m.load(ctx, m.OAuthKey(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrRevoked
	}
	return s, err
}

// NewOAuthSession binds a fresh login to a new OAuth token. The token itself
// is returned to the client once and only its HMAC is stored.
func (m *Manager) NewOAuthSession(ctx context.Context, res *LoginResult, consumerKey string, userID int64) (token, tokenSecret string, s *Session, err error) {
	token = fmt.Sprintf("%d-%s", userID, secret.Token(32))
	tokenSecret = secret.Token(32)
	s, err = m.persist(ctx, m.OAuthKey(token), KindOAuth, res, consumerKey, tokenSecret)
	return token, tokenSecret, s, err
}

// LoginResult is a verified app-password login.
type LoginResult struct {
	Identity *ident.Identity
	Session  *atp.Session
	Scope    string
}

// Login resolves the account, calls createSession on its PDS and enforces
// app-password-only access. It applies brute-force limits per IP and per
// account.
func (m *Manager) Login(ctx context.Context, ip, identifier, password string) (*LoginResult, error) {
	id := ident.NormalizeIdentifier(identifier, m.o.DefaultHandleHost)
	ipKey, idKey := "ip:"+ip, "id:"+id
	if locked, _ := m.ipFails.Locked(ipKey); locked {
		return nil, ErrLocked
	}
	if locked, _ := m.idFails.Locked(idKey); locked {
		return nil, ErrLocked
	}
	fail := func(err error) (*LoginResult, error) {
		m.ipFails.Fail(ipKey)
		m.idFails.Fail(idKey)
		return nil, err
	}
	who, err := m.o.Resolver.Resolve(ctx, id)
	if err != nil {
		if errors.Is(err, ident.ErrNotFound) {
			return fail(ErrUnknownAccount)
		}
		return nil, fmt.Errorf("resolving %s: %w", id, err)
	}
	c := m.Client(who.PDS)
	var out atp.Session
	err = c.Do(ctx, &atp.Request{Method: http.MethodPost, NSID: "com.atproto.server.createSession",
		Body: map[string]string{"identifier": who.DID, "password": password}}, &out)
	if err != nil {
		if atp.IsError(err, "AuthFactorTokenRequired") {
			// Only account passwords trigger email 2FA; app passwords never do.
			return nil, ErrFullPassword
		}
		if atp.Status(err) == http.StatusUnauthorized || atp.IsError(err, "AuthenticationRequired") {
			return fail(ErrBadCredentials)
		}
		return nil, fmt.Errorf("createSession: %w", err)
	}
	if out.DID != who.DID {
		m.revoke(ctx, c, out.RefreshJWT)
		return nil, fmt.Errorf("createSession returned DID %s for %s", out.DID, who.DID)
	}
	scope, err := m.checkScope(ctx, c, &out)
	if err != nil {
		m.revoke(ctx, c, out.RefreshJWT)
		// A correct (but full-access) password is not a brute-force failure.
		return nil, err
	}
	m.ipFails.Succeed(ipKey)
	m.idFails.Succeed(idKey)
	if out.Handle != "" {
		who.Handle = out.Handle
	}
	return &LoginResult{Identity: who, Session: &out, Scope: scope}, nil
}

// checkScope reads the access JWT's scope claim. If the token is not a
// readable JWT, it probes an endpoint only full-access sessions may call.
func (m *Manager) checkScope(ctx context.Context, c *atp.Client, s *atp.Session) (string, error) {
	claims, ok := decodeJWT(s.AccessJWT)
	if ok && claims.Scope != "" {
		switch claims.Scope {
		case ScopeAppPass, ScopeAppPassPrivileged:
			return claims.Scope, nil
		case ScopeFullAccess:
			return "", ErrFullPassword
		}
		return "", fmt.Errorf("%w (unrecognised token scope %q)", ErrFullPassword, claims.Scope)
	}
	err := c.Do(ctx, &atp.Request{NSID: "com.atproto.server.listAppPasswords", Token: s.AccessJWT}, nil)
	switch {
	case err == nil:
		return "", ErrFullPassword
	case atp.Status(err) == http.StatusBadRequest || atp.Status(err) == http.StatusUnauthorized || atp.Status(err) == http.StatusForbidden:
		return ScopeAppPass, nil
	}
	return "", fmt.Errorf("checking app password scope: %w", err)
}

func (m *Manager) revoke(ctx context.Context, c *atp.Client, refresh string) {
	if refresh == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = c.Do(ctx, &atp.Request{Method: http.MethodPost, NSID: "com.atproto.server.deleteSession", Token: refresh}, nil)
}

type jwtClaims struct {
	Scope string `json:"scope"`
	Exp   int64  `json:"exp"`
	Sub   string `json:"sub"`
}

// decodeJWT reads (without verifying) a JWT payload. The token came straight
// from the PDS over TLS; we only need its declared scope and expiry.
func decodeJWT(tok string) (jwtClaims, bool) {
	var c jwtClaims
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return c, false
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return c, false
	}
	return c, json.Unmarshal(b, &c) == nil
}

func (m *Manager) persist(ctx context.Context, key []byte, kind string, res *LoginResult, consumerKey, tokenSecret string) (*Session, error) {
	now := m.now()
	s := &Session{
		Key: key, Kind: kind, DID: res.Identity.DID, PDS: res.Identity.PDS, Scope: res.Scope,
		ConsumerKey: consumerKey, TokenSecret: tokenSecret, mgr: m,
		handle: res.Identity.Handle, access: res.Session.AccessJWT, refresh: res.Session.RefreshJWT,
		accessExp: accessExpiry(res.Session.AccessJWT, now), touched: now,
	}
	row, err := m.seal(s)
	if err != nil {
		return nil, err
	}
	row.CreatedAt, row.LastUsedAt = now, now
	if err := m.o.Store.PutSession(ctx, row); err != nil {
		return nil, err
	}
	m.live.Add(string(key), s)
	return s, nil
}

func (m *Manager) seal(s *Session) (*store.SessionRow, error) {
	r := &store.SessionRow{Key: s.Key, Kind: s.Kind, DID: s.DID, Handle: s.handle, PDS: s.PDS, Scope: s.Scope, ConsumerKey: s.ConsumerKey}
	var err error
	if r.AccessEnc, err = m.o.Keys.Seal([]byte(s.access), append([]byte("access:"), s.Key...)); err != nil {
		return nil, err
	}
	if r.RefreshEnc, err = m.o.Keys.Seal([]byte(s.refresh), append([]byte("refresh:"), s.Key...)); err != nil {
		return nil, err
	}
	if s.TokenSecret != "" {
		if r.TokenSecretEnc, err = m.o.Keys.Seal([]byte(s.TokenSecret), append([]byte("tsecret:"), s.Key...)); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// load returns a live session from memory or disk.
func (m *Manager) load(ctx context.Context, key []byte) (*Session, error) {
	if s, ok := m.live.Get(string(key)); ok {
		m.touch(ctx, s)
		return s, nil
	}
	row, err := m.o.Store.GetSession(ctx, key)
	if err != nil {
		return nil, err
	}
	refresh, err := m.o.Keys.Open(row.RefreshEnc, append([]byte("refresh:"), key...))
	if err != nil {
		// Wrong encryption key or tampering: treat as revoked.
		_ = m.o.Store.DeleteSession(ctx, key)
		return nil, ErrRevoked
	}
	s := &Session{Key: key, Kind: row.Kind, DID: row.DID, PDS: row.PDS, Scope: row.Scope, ConsumerKey: row.ConsumerKey,
		mgr: m, handle: row.Handle, refresh: string(refresh), touched: row.LastUsedAt}
	if access, err := m.o.Keys.Open(row.AccessEnc, append([]byte("access:"), key...)); err == nil {
		s.access = string(access)
		s.accessExp = accessExpiry(s.access, m.now())
	}
	if len(row.TokenSecretEnc) > 0 {
		if ts, err := m.o.Keys.Open(row.TokenSecretEnc, append([]byte("tsecret:"), key...)); err == nil {
			s.TokenSecret = string(ts)
		}
	}
	m.live.Add(string(key), s)
	m.touch(ctx, s)
	return s, nil
}

// touch persists last-used time at most hourly per session.
func (m *Manager) touch(ctx context.Context, s *Session) {
	now := m.now()
	s.mu.Lock()
	stale := now.Sub(s.touched) > time.Hour
	if stale {
		s.touched = now
	}
	s.mu.Unlock()
	if stale {
		_ = m.o.Store.TouchSession(ctx, s.Key, now)
	}
}

// Drop forgets a session (logout or revocation).
func (m *Manager) Drop(ctx context.Context, s *Session) {
	m.live.Remove(string(s.Key))
	_ = m.o.Store.DeleteSession(ctx, s.Key)
}

func accessExpiry(tok string, now time.Time) time.Time {
	if c, ok := decodeJWT(tok); ok && c.Exp > 0 {
		return time.Unix(c.Exp, 0)
	}
	return now.Add(30 * time.Minute)
}

// token returns a valid access token, refreshing if it expires within a minute.
func (s *Session) token(ctx context.Context, force bool) (string, error) {
	s.mu.Lock()
	tok, exp := s.access, s.accessExp
	s.mu.Unlock()
	if !force && tok != "" && s.mgr.now().Add(time.Minute).Before(exp) {
		return tok, nil
	}
	v, err, _ := s.mgr.sf.Do("refresh:"+string(s.Key), func() (any, error) {
		// Another caller may have refreshed while we waited.
		s.mu.Lock()
		if s.access != tok && s.mgr.now().Add(time.Minute).Before(s.accessExp) {
			t := s.access
			s.mu.Unlock()
			return t, nil
		}
		refresh := s.refresh
		s.mu.Unlock()
		return s.doRefresh(ctx, refresh)
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

func (s *Session) doRefresh(ctx context.Context, refresh string) (string, error) {
	m := s.mgr
	// Refresh must not be cancelled half-way by one client's disconnect:
	// refresh tokens rotate, and losing the new one would log everyone out.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	var out atp.Session
	err := m.Client(s.PDS).Do(ctx, &atp.Request{Method: http.MethodPost, NSID: "com.atproto.server.refreshSession", Token: refresh}, &out)
	if err != nil {
		if st := atp.Status(err); st == http.StatusBadRequest || st == http.StatusUnauthorized {
			m.Drop(ctx, s)
			return "", ErrRevoked
		}
		return "", err
	}
	now := m.now()
	s.mu.Lock()
	s.access, s.refresh = out.AccessJWT, out.RefreshJWT
	s.accessExp = accessExpiry(out.AccessJWT, now)
	if out.Handle != "" {
		s.handle = out.Handle
	}
	s.mu.Unlock()
	row, err := m.seal(s)
	if err == nil {
		err = m.o.Store.UpdateSessionTokens(ctx, s.Key, row.Handle, s.PDS, row.AccessEnc, row.RefreshEnc, now)
	}
	if err != nil {
		m.o.Logger.Error("persisting refreshed session", "err", err)
	}
	return out.AccessJWT, nil
}

// Do makes an authenticated call as this session, adding the service-proxy
// header for AppView and chat methods and refreshing expired tokens once.
func (s *Session) Do(ctx context.Context, r *atp.Request, out any) error {
	if r.Proxy == "" {
		switch {
		case strings.HasPrefix(r.NSID, "app.bsky."):
			r.Proxy = s.mgr.o.AppViewProxy
		case strings.HasPrefix(r.NSID, "chat.bsky."):
			r.Proxy = s.mgr.o.ChatProxy
		}
	}
	// Buffer reader bodies so a retry after refresh can resend them.
	var raw []byte
	if rd, ok := r.Body.(io.Reader); ok {
		b, err := io.ReadAll(rd)
		if err != nil {
			return err
		}
		raw = b
		r.Body = bytes.NewReader(raw)
	}
	tok, err := s.token(ctx, false)
	if err != nil {
		return err
	}
	r.Token = tok
	c := s.mgr.Client(s.PDS)
	err = c.Do(ctx, r, out)
	if atp.IsError(err, "ExpiredToken", "InvalidToken") || (atp.Status(err) == http.StatusUnauthorized && !atp.IsError(err, "AuthMissing")) {
		if tok, err = s.token(ctx, true); err != nil {
			return err
		}
		r.Token = tok
		if raw != nil {
			r.Body = bytes.NewReader(raw)
		}
		err = c.Do(ctx, r, out)
	}
	return err
}

// Logout revokes the session upstream and forgets it.
func (s *Session) Logout(ctx context.Context) {
	s.mu.Lock()
	refresh := s.refresh
	s.mu.Unlock()
	s.mgr.revoke(ctx, s.mgr.Client(s.PDS), refresh)
	s.mgr.Drop(ctx, s)
}
