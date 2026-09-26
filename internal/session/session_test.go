package session

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/j4ckxyz/mockingbird/internal/atp"
	"github.com/j4ckxyz/mockingbird/internal/fakepds"
	"github.com/j4ckxyz/mockingbird/internal/ident"
	"github.com/j4ckxyz/mockingbird/internal/netguard"
	"github.com/j4ckxyz/mockingbird/internal/secret"
	"github.com/j4ckxyz/mockingbird/internal/store"
)

type env struct {
	pds *fakepds.Server
	mgr *Manager
	st  *store.Store
}

func setup(t *testing.T) *env {
	t.Helper()
	pds := fakepds.New()
	srv := httptest.NewServer(pds)
	t.Cleanup(srv.Close)
	pds.URL = srv.URL
	pds.AddAccount(&fakepds.Account{DID: "did:plc:alice", Handle: "alice.test", DisplayName: "Alice",
		AppPassword: "aaaa-bbbb-cccc-dddd", DMPassword: "dmdm-dmdm-dmdm-dmdm", Password: "correct horse"})
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	keys, _ := secret.New(bytes.Repeat([]byte{7}, 32), bytes.Repeat([]byte{9}, 32))
	hc := netguard.NewClient(netguard.Options{AllowPrivate: true, PlainHTTPHosts: []string{srv.Listener.Addr().String()}})
	mgr := NewManager(Options{Store: st, Keys: keys, Resolver: ident.NewFromDirectory(pds.Directory()), HTTP: hc,
		AppViewProxy: "did:web:api.bsky.app#bsky_appview", DefaultHandleHost: "test"})
	return &env{pds: pds, mgr: mgr, st: st}
}

func TestBasicLoginCachesSession(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	s1, err := e.mgr.Basic(ctx, "1.2.3.4", "alice", "aaaa-bbbb-cccc-dddd")
	if err != nil {
		t.Fatal(err)
	}
	if s1.DID != "did:plc:alice" || s1.Scope != ScopeAppPass || s1.CanDM() {
		t.Fatalf("unexpected session %+v", s1)
	}
	for i := 0; i < 5; i++ {
		if _, err := e.mgr.Basic(ctx, "1.2.3.4", "@Alice.test", "aaaa-bbbb-cccc-dddd"); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.pds.CallCount("com.atproto.server.createSession"); n != 1 {
		t.Fatalf("createSession called %d times, want 1", n)
	}
	// A new manager (process restart) reuses the stored session.
	mgr2 := NewManager(e.mgr.o)
	if _, err := mgr2.Basic(ctx, "1.2.3.4", "alice.test", "aaaa-bbbb-cccc-dddd"); err != nil {
		t.Fatal(err)
	}
	if n := e.pds.CallCount("com.atproto.server.createSession"); n != 1 {
		t.Fatalf("createSession after restart called %d times, want 1", n)
	}
}

func TestPrivilegedScope(t *testing.T) {
	e := setup(t)
	s, err := e.mgr.Basic(context.Background(), "ip", "alice.test", "dmdm-dmdm-dmdm-dmdm")
	if err != nil || !s.CanDM() {
		t.Fatalf("want DM-capable session, got %v %v", s, err)
	}
}

func TestFullPasswordRejected(t *testing.T) {
	e := setup(t)
	_, err := e.mgr.Basic(context.Background(), "ip", "alice.test", "correct horse")
	if !errors.Is(err, ErrFullPassword) {
		t.Fatalf("want ErrFullPassword, got %v", err)
	}
	if e.pds.CallCount("com.atproto.server.deleteSession") != 1 {
		t.Fatal("full-access session was not revoked upstream")
	}
	n, _ := e.st.CountSessions(context.Background())
	if n != 0 {
		t.Fatal("full-access session was stored")
	}
}

func TestWrongPasswordAndLockout(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, err := e.mgr.Basic(ctx, "9.9.9.9", "alice.test", "wrong-pass-word-xxxx")
		if !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("attempt %d: want ErrBadCredentials, got %v", i, err)
		}
	}
	_, err := e.mgr.Basic(ctx, "9.9.9.9", "alice.test", "aaaa-bbbb-cccc-dddd")
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("want ErrLocked after repeated failures, got %v", err)
	}
	calls := e.pds.CallCount("com.atproto.server.createSession")
	e.mgr.Basic(ctx, "9.9.9.9", "alice.test", "wrong-pass-word-xxxx")
	if e.pds.CallCount("com.atproto.server.createSession") != calls {
		t.Fatal("createSession called while locked out")
	}
}

func TestExistingSessionSurvivesLockout(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.mgr.Basic(ctx, "1.1.1.1", "alice.test", "aaaa-bbbb-cccc-dddd"); err != nil {
		t.Fatal(err)
	}
	// An attacker hammering the handle from elsewhere must not lock the owner out.
	for i := 0; i < 10; i++ {
		e.mgr.Basic(ctx, "6.6.6.6", "alice.test", "nope-nope-nope-nope")
	}
	if _, err := e.mgr.Basic(ctx, "1.1.1.1", "alice.test", "aaaa-bbbb-cccc-dddd"); err != nil {
		t.Fatalf("cached session should still work: %v", err)
	}
}

func TestRefreshAndRevocation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.pds.AccessTTL = 30 * time.Second // below the one-minute refresh margin
	s, err := e.mgr.Basic(ctx, "ip", "alice.test", "aaaa-bbbb-cccc-dddd")
	if err != nil {
		t.Fatal(err)
	}
	var out atp.Profile
	if err := s.Do(ctx, &atp.Request{NSID: "app.bsky.actor.getProfile", Params: map[string][]string{"actor": {s.DID}}}, &out); err != nil {
		t.Fatal(err)
	}
	if e.pds.CallCount("com.atproto.server.refreshSession") == 0 {
		t.Fatal("expected a refresh for a nearly expired token")
	}
	// Revoke upstream: the next refresh fails and the session is dropped.
	e.pds.RevokeAll()
	err = s.Do(ctx, &atp.Request{NSID: "app.bsky.actor.getProfile", Params: map[string][]string{"actor": {s.DID}}}, &out)
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("want ErrRevoked, got %v", err)
	}
	// Basic Auth still holds the password, so it logs in again transparently.
	e.pds.AccessTTL = time.Hour
	if _, err := e.mgr.Basic(ctx, "ip", "alice.test", "aaaa-bbbb-cccc-dddd"); err != nil {
		t.Fatalf("re-login after revocation: %v", err)
	}
}

func TestRefreshTokensEncryptedAtRest(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	s, err := e.mgr.Basic(ctx, "ip", "alice.test", "aaaa-bbbb-cccc-dddd")
	if err != nil {
		t.Fatal(err)
	}
	row, err := e.st.GetSession(ctx, s.Key)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(row.RefreshEnc, []byte("eyJ")) || bytes.Contains(row.AccessEnc, []byte("eyJ")) {
		t.Fatal("tokens stored in plaintext")
	}
	if bytes.Contains(s.Key, []byte("aaaa")) {
		t.Fatal("session key contains the password")
	}
}

func TestOAuthSession(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	res, err := e.mgr.Login(ctx, "ip", "alice.test", "aaaa-bbbb-cccc-dddd")
	if err != nil {
		t.Fatal(err)
	}
	tok, sec, _, err := e.mgr.NewOAuthSession(ctx, res, "consumer", 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 43 || tok[:3] != "42-" || len(sec) != 43 {
		t.Fatalf("unexpected token shape %q %q", tok, sec)
	}
	s, err := NewManager(e.mgr.o).OAuth(ctx, tok)
	if err != nil || s.DID != "did:plc:alice" || s.TokenSecret != sec {
		t.Fatalf("oauth lookup: %+v %v", s, err)
	}
	if _, err := e.mgr.OAuth(ctx, tok+"x"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("bad token: want ErrRevoked, got %v", err)
	}
}
