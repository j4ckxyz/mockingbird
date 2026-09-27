package api_test

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/j4ckxyz/mockingbird/internal/api"
	"github.com/j4ckxyz/mockingbird/internal/atp"
	"github.com/j4ckxyz/mockingbird/internal/config"
	"github.com/j4ckxyz/mockingbird/internal/fakepds"
	"github.com/j4ckxyz/mockingbird/internal/ident"
	"github.com/j4ckxyz/mockingbird/internal/media"
	"github.com/j4ckxyz/mockingbird/internal/metrics"
	"github.com/j4ckxyz/mockingbird/internal/netguard"
	"github.com/j4ckxyz/mockingbird/internal/secret"
	"github.com/j4ckxyz/mockingbird/internal/session"
	"github.com/j4ckxyz/mockingbird/internal/store"
	"github.com/j4ckxyz/mockingbird/internal/web"
)

// harnessNow, when set, fixes the API server's clock.
var harnessNow func() time.Time

const (
	alicePW = "aaaa-bbbb-cccc-dddd"
	aliceDM = "dmdm-dmdm-dmdm-dmdm"
	bobPW   = "bbbb-bbbb-bbbb-bbbb"
	bobDM   = "bdmb-bdmb-bdmb-bdmb"
)

type harness struct {
	t   *testing.T
	pds *fakepds.Server
	srv *httptest.Server
	api *api.Server
	st  *store.Store
	cfg *config.Config
}

func newHarness(t *testing.T, mutate ...func(map[string]string)) *harness {
	t.Helper()
	return newHarnessWithAccounts(t, nil, mutate...)
}

// newHarnessWithAccounts adds accounts before the identity directory is
// built (the fake directory is a snapshot).
func newHarnessWithAccounts(t *testing.T, extra []*fakepds.Account, mutate ...func(map[string]string)) *harness {
	t.Helper()
	pds := fakepds.New()
	pdsSrv := httptest.NewServer(pds)
	t.Cleanup(pdsSrv.Close)
	pds.URL = pdsSrv.URL
	pds.AddAccount(&fakepds.Account{DID: "did:plc:alice", Handle: "alice.test", DisplayName: "Alice <A>",
		AppPassword: alicePW, DMPassword: aliceDM, Password: "full-password", Follows: []string{"did:plc:bob"}})
	pds.AddAccount(&fakepds.Account{DID: "did:plc:bob", Handle: "bob.test", DisplayName: "Bob",
		AppPassword: bobPW, DMPassword: bobDM, Follows: []string{"did:plc:alice"}})
	pds.AddAccount(&fakepds.Account{DID: "did:plc:carol", Handle: "carol.other.example", DisplayName: "Carol"})
	for _, a := range extra {
		pds.AddAccount(a)
	}

	dir := t.TempDir()
	env := map[string]string{
		"MB_SECRET_KEY":          strings.Repeat("11", 32),
		"MB_ENCRYPTION_KEY":      strings.Repeat("22", 32),
		"MB_DATA_DIR":            dir,
		"MB_PUBLIC_URL":          "http://bird.test",
		"MB_DEFAULT_HANDLE_HOST": "test",
		"MB_ADMIN_ADDR":          "127.0.0.1:0",
		"MB_PER_IP_RATE":         "1000",
		"MB_PER_IP_BURST":        "1000",
		"MB_PER_ACCOUNT_RATE":    "1000",
		"MB_PER_ACCOUNT_BURST":   "1000",
		"MB_RESPONSE_CACHE_TTL":  "1ns",
	}
	for _, f := range mutate {
		f(env)
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	keys, _ := secret.New(cfg.SecretKey, cfg.EncryptionKey)
	hc := netguard.NewClient(netguard.Options{AllowPrivate: true, PlainHTTPHosts: []string{pdsSrv.Listener.Addr().String()}, MaxBodyBytes: 8 << 20})
	resolver := ident.NewFromDirectory(pds.Directory())
	m := metrics.New()
	sessions := session.NewManager(session.Options{Store: st, Keys: keys, Resolver: resolver, HTTP: hc,
		AppViewProxy: cfg.AppViewProxy, ChatProxy: cfg.ChatProxy, DefaultHandleHost: cfg.DefaultHandleHost})
	signer := media.NewSigner(keys.MAC("image"), cfg.PublicURL.String())
	cache, _ := media.OpenDiskCache(filepath.Join(dir, "img"), 1<<20)
	public := &atp.Client{HTTP: hc, Host: pdsSrv.URL}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := api.New(api.Deps{Config: cfg, Store: st, Keys: keys, Sessions: sessions, Resolver: resolver, Signer: signer,
		Images: &media.Proxy{Signer: signer, CDN: pdsSrv.URL, HTTP: hc, Cache: cache, Logger: quiet}, Public: public, Metrics: m, Logger: quiet,
		Now: harnessNow})
	a.SetWeb(web.New(web.Deps{Config: cfg, API: a, Store: st, Public: public, Signer: signer, Logger: quiet}))
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &harness{t: t, pds: pds, srv: srv, api: a, st: st, cfg: cfg}
}

type resp struct {
	code int
	hdr  http.Header
	body []byte
}

func (r resp) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("bad JSON (%d): %v\n%s", r.code, err, r.body)
	}
}

type reqOpt func(*http.Request)

func basic(user, pass string) reqOpt {
	return func(r *http.Request) {
		r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	}
}

func host(h string) reqOpt { return func(r *http.Request) { r.Host = h } }

func header(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

func (h *harness) do(method, path string, form url.Values, opts ...reqOpt) resp {
	h.t.Helper()
	var body io.Reader
	if form != nil && method != http.MethodGet {
		body = strings.NewReader(form.Encode())
	} else if form != nil {
		path += "?" + form.Encode()
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, o := range opts {
		o(req)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{DisableCompression: true}}
	res, err := c.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			h.t.Fatal(err)
		}
		b, _ = io.ReadAll(zr)
	}
	return resp{code: res.StatusCode, hdr: res.Header, body: b}
}

func (h *harness) get(path string, opts ...reqOpt) resp { return h.do("GET", path, nil, opts...) }

func (h *harness) post(path string, form url.Values, opts ...reqOpt) resp {
	return h.do("POST", path, form, opts...)
}

var alice = basic("alice.test", alicePW)

// seed adds a few posts: bob's two posts and alice's one.
func (h *harness) seed() (bob1, bob2, alice1 *fakepds.Post) {
	base := time.Now().Add(-time.Hour)
	bob1 = h.pds.AddPost("did:plc:bob", "first post from bob", base)
	alice1 = h.pds.AddPost("did:plc:alice", "alice says hi & <waves>", base.Add(10*time.Minute))
	bob2 = h.pds.AddPost("did:plc:bob", "second post from bob", base.Add(20*time.Minute))
	return
}
