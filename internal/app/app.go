// Package app assembles the bridge from configuration. It is shared by the
// binary, the integration tests and the load-test tool.
package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"net/netip"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	"github.com/jackgilbert/mockingbird/internal/api"
	"github.com/jackgilbert/mockingbird/internal/atp"
	"github.com/jackgilbert/mockingbird/internal/config"
	"github.com/jackgilbert/mockingbird/internal/ident"
	"github.com/jackgilbert/mockingbird/internal/media"
	"github.com/jackgilbert/mockingbird/internal/metrics"
	"github.com/jackgilbert/mockingbird/internal/netguard"
	"github.com/jackgilbert/mockingbird/internal/secret"
	"github.com/jackgilbert/mockingbird/internal/session"
	"github.com/jackgilbert/mockingbird/internal/store"
	"github.com/jackgilbert/mockingbird/internal/tlslegacy"
	"github.com/jackgilbert/mockingbird/internal/web"
)

// UserAgent identifies the bridge upstream.
const UserAgent = "mockingbird-bridge (+https://github.com/jackgilbert/mockingbird)"

// Options override pieces for tests and tools.
type Options struct {
	Logger *slog.Logger
	// Directory replaces identity resolution (tests, load tests).
	Directory identity.Directory
	// PlainHTTPHosts may be reached over http (tests, load tests).
	PlainHTTPHosts []string
}

// App is an assembled bridge.
type App struct {
	Config   *config.Config
	Handler  http.Handler // public API and pages
	Admin    http.Handler // metrics, pprof, unknown endpoints
	TLS      *tls.Config  // legacy listener config, nil when disabled
	Metrics  *metrics.Metrics
	API      *api.Server
	Store    *store.Store
	Log      *slog.Logger
	sessions atomic.Int64
}

// New builds the bridge.
func New(cfg *config.Config, o Options) (*App, error) {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	keys, err := secret.New(cfg.SecretKey, cfg.EncryptionKey)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	m := metrics.New()

	guard := func(timeout time.Duration, maxBody int64, redirects int, plain ...string) *http.Client {
		return netguard.NewClient(netguard.Options{Timeout: timeout, MaxBodyBytes: maxBody, MaxRedirects: redirects,
			PlainHTTPHosts: append(plain, o.PlainHTTPHosts...), AllowPrivate: cfg.InsecureAllowPrivateNetworks, UserAgent: UserAgent})
	}
	var plcPlain []string
	if u, err := url.Parse(cfg.PLCURL); err == nil && u.Scheme == "http" {
		plcPlain = []string{u.Host}
	}
	identHTTP := guard(5*time.Second, 64<<10, 3)
	plcHTTP := guard(5*time.Second, 256<<10, 0, plcPlain...)
	pdsHTTP := guard(20*time.Second, 8<<20, 2)
	cdnHTTP := guard(15*time.Second, 12<<20, 2)

	var dir *ident.Directory
	if o.Directory != nil {
		dir = ident.NewFromDirectory(o.Directory)
	} else {
		dir = ident.New(ident.Options{PLCURL: cfg.PLCURL, HTTPClient: identHTTP, PLCClient: plcHTTP,
			HitTTL: cfg.IdentityCacheTTL, ErrTTL: 5 * time.Minute, UserAgent: UserAgent})
	}
	sessions := session.NewManager(session.Options{
		Store: st, Keys: keys, Resolver: dir, HTTP: pdsHTTP, Hook: m.ObserveUpstream,
		AppViewProxy: cfg.AppViewProxy, ChatProxy: cfg.ChatProxy, DefaultHandleHost: cfg.DefaultHandleHost, Logger: log,
	})
	signer := media.NewSigner(keys.MAC("image-url-signing"), cfg.PublicURL.String())
	imgCache, err := media.OpenDiskCache(filepath.Join(cfg.DataDir, "imgcache"), cfg.ImageCacheBytes)
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("opening image cache: %w", err)
	}
	images := &media.Proxy{Signer: signer, CDN: cfg.ImageCDNURL, HTTP: cdnHTTP, Cache: imgCache, Logger: log, Hook: m.Image}
	public := &atp.Client{HTTP: pdsHTTP, Host: cfg.PublicAppViewURL, Hook: m.ObserveUpstream}

	a := &App{Config: cfg, Metrics: m, Store: st, Log: log}
	var tlsMat *tlslegacy.Material
	if cfg.LegacyTLSAddr != "" {
		hosts := append(append([]string{}, cfg.BridgeHosts...), cfg.TLSExtraHosts...)
		tlsMat, err = tlslegacy.Load(tlslegacy.Options{Dir: filepath.Join(cfg.DataDir, "tls"), CommonName: cfg.TLSCommonName,
			Hosts: hosts, NameConstraints: cfg.TLSNameConstraints, SHA1: cfg.TLSSHA1})
		if err != nil {
			st.Close()
			return nil, err
		}
		a.TLS = tlsMat.Config()
	}
	caDER := func() []byte {
		if tlsMat == nil {
			return nil
		}
		return tlsMat.CADER
	}
	apiSrv := api.New(api.Deps{Config: cfg, Store: st, Keys: keys, Sessions: sessions, Resolver: dir, Signer: signer,
		Images: images, Public: public, Metrics: m, Logger: log, CA: caDER})
	apiSrv.SetWeb(web.New(web.Deps{Config: cfg, API: apiSrv, Store: st, Public: public, Signer: signer, Logger: log, CA: caDER}))
	a.API, a.Handler = apiSrv, apiSrv
	a.Admin = adminHandler(m, apiSrv, cfg)

	m.Gauge("mockingbird_sessions", "Stored sessions.", func() float64 { return float64(a.sessions.Load()) })
	m.Gauge("mockingbird_image_cache_bytes", "Image cache size in bytes.", func() float64 { b, _ := imgCache.Size(); return float64(b) })
	return a, nil
}

// Close releases resources.
func (a *App) Close() error { return a.Store.Close() }

// Maintain purges idle sessions and stale uploads hourly until ctx ends.
func (a *App) Maintain(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := a.Store.PurgeSessions(ctx, time.Now().Add(-a.Config.SessionIdleExpiry)); err == nil && n > 0 {
			a.Log.Info("purged idle sessions", "count", n)
		}
		a.Store.PurgeUploads(ctx, time.Now().Add(-7*24*time.Hour))
		if n, err := a.Store.CountSessions(ctx); err == nil {
			a.sessions.Store(n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// adminHandler serves metrics, pprof and the unknown-endpoint report. Every
// request is re-checked against the peer address, in addition to binding
// only loopback or Tailscale addresses.
func adminHandler(m *metrics.Metrics, apiSrv *api.Server, cfg *config.Config) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/admin/unknown", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(apiSrv.Unknown())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		a, err := netip.ParseAddr(host)
		if err != nil || !cfg.AdminPeerAllowed(a) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
