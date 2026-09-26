// Command mockingbird is a bridge that lets 2009-era Twitter clients use
// Bluesky, by serving the Twitter REST API v1 and Search API and translating
// each request into AT Protocol calls against the user's own PDS.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackgilbert/mockingbird/internal/api"
	"github.com/jackgilbert/mockingbird/internal/atp"
	"github.com/jackgilbert/mockingbird/internal/config"
	"github.com/jackgilbert/mockingbird/internal/ident"
	"github.com/jackgilbert/mockingbird/internal/logging"
	"github.com/jackgilbert/mockingbird/internal/media"
	"github.com/jackgilbert/mockingbird/internal/metrics"
	"github.com/jackgilbert/mockingbird/internal/netguard"
	"github.com/jackgilbert/mockingbird/internal/secret"
	"github.com/jackgilbert/mockingbird/internal/session"
	"github.com/jackgilbert/mockingbird/internal/store"
	"github.com/jackgilbert/mockingbird/internal/tlslegacy"
	"github.com/jackgilbert/mockingbird/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const userAgent = "mockingbird-bridge (+https://github.com/jackgilbert/mockingbird)"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(healthcheck())
		case "genkeys":
			for _, name := range []string{"MB_SECRET_KEY", "MB_ENCRYPTION_KEY"} {
				b := make([]byte, 32)
				rand.Read(b)
				fmt.Printf("%s=%s\n", name, hex.EncodeToString(b))
			}
			return
		case "version":
			fmt.Println(version)
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mockingbird:", err)
		os.Exit(1)
	}
}

// healthcheck probes the local HTTP listener (the image has no curl).
func healthcheck() int {
	addr := os.Getenv("MB_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return 1
	}
	return 0
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logging.New(os.Stderr, cfg.LogLevel)
	slog.SetDefault(log)
	log.Info("starting", "version", version, "public_url", cfg.PublicURL.String(), "http", cfg.HTTPAddr,
		"legacy_tls", cfg.LegacyTLSAddr, "admin", strings.Join(cfg.AdminAddrs, ","))
	if cfg.InsecureAllowPrivateNetworks {
		log.Warn("MB_INSECURE_ALLOW_PRIVATE_NETWORKS is set: the SSRF guard is disabled. Never use this on a public deployment.")
	}

	keys, err := secret.New(cfg.SecretKey, cfg.EncryptionKey)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer st.Close()
	m := metrics.New()

	guard := func(timeout time.Duration, maxBody int64, redirects int, plain ...string) *http.Client {
		return netguard.NewClient(netguard.Options{Timeout: timeout, MaxBodyBytes: maxBody, MaxRedirects: redirects,
			PlainHTTPHosts: plain, AllowPrivate: cfg.InsecureAllowPrivateNetworks, UserAgent: userAgent})
	}
	var plcPlain []string
	if u, err := url.Parse(cfg.PLCURL); err == nil && u.Scheme == "http" {
		plcPlain = []string{u.Host}
	}
	identHTTP := guard(5*time.Second, 64<<10, 3)
	plcHTTP := guard(5*time.Second, 256<<10, 0, plcPlain...)
	pdsHTTP := guard(20*time.Second, 8<<20, 2)
	cdnHTTP := guard(15*time.Second, 12<<20, 2)

	dir := ident.New(ident.Options{PLCURL: cfg.PLCURL, HTTPClient: identHTTP, PLCClient: plcHTTP,
		HitTTL: cfg.IdentityCacheTTL, ErrTTL: 5 * time.Minute, UserAgent: userAgent})
	sessions := session.NewManager(session.Options{
		Store: st, Keys: keys, Resolver: dir, HTTP: pdsHTTP, Hook: m.ObserveUpstream,
		AppViewProxy: cfg.AppViewProxy, ChatProxy: cfg.ChatProxy, DefaultHandleHost: cfg.DefaultHandleHost, Logger: log,
	})
	signer := media.NewSigner(keys.MAC("image-url-signing"), cfg.PublicURL.String())
	imgCache, err := media.OpenDiskCache(filepath.Join(cfg.DataDir, "imgcache"), cfg.ImageCacheBytes)
	if err != nil {
		return fmt.Errorf("opening image cache: %w", err)
	}
	images := &media.Proxy{Signer: signer, CDN: cfg.ImageCDNURL, HTTP: cdnHTTP, Cache: imgCache, Logger: log, Hook: m.Image}
	public := &atp.Client{HTTP: pdsHTTP, Host: cfg.PublicAppViewURL, Hook: m.ObserveUpstream}

	var tlsMat *tlslegacy.Material
	if cfg.LegacyTLSAddr != "" {
		hosts := append(append([]string{}, cfg.BridgeHosts...), cfg.TLSExtraHosts...)
		tlsMat, err = tlslegacy.Load(tlslegacy.Options{Dir: filepath.Join(cfg.DataDir, "tls"), CommonName: cfg.TLSCommonName,
			Hosts: hosts, NameConstraints: cfg.TLSNameConstraints, SHA1: cfg.TLSSHA1})
		if err != nil {
			return err
		}
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

	var sessionCount atomic.Int64
	m.Gauge("mockingbird_sessions", "Stored sessions.", func() float64 { return float64(sessionCount.Load()) })
	m.Gauge("mockingbird_image_cache_bytes", "Image cache size in bytes.", func() float64 { b, _ := imgCache.Size(); return float64(b) })

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go maintenance(ctx, log, st, cfg, &sessionCount)

	errLog := slog.NewLogLogger(log.Handler(), slog.LevelWarn)
	newServer := func(addr string, h http.Handler) *http.Server {
		return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
			WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 32 << 10, ErrorLog: errLog}
	}
	var servers []*http.Server
	errc := make(chan error, 8)

	httpSrv := newServer(cfg.HTTPAddr, apiSrv)
	servers = append(servers, httpSrv)
	go func() { errc <- listen(httpSrv, nil) }()

	if tlsMat != nil {
		tlsSrv := newServer(cfg.LegacyTLSAddr, apiSrv)
		tlsSrv.TLSConfig = tlsMat.Config()
		tlsSrv.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){} // HTTP/1.1 only
		servers = append(servers, tlsSrv)
		go func() { errc <- listen(tlsSrv, tlsSrv.TLSConfig) }()
		log.Info("legacy TLS listener enabled", "addr", cfg.LegacyTLSAddr, "cn", cfg.TLSCommonName)
	}

	admin := adminHandler(m, apiSrv)
	for _, a := range cfg.AdminAddrs {
		srv := newServer(a, admin)
		servers = append(servers, srv)
		go listenRetry(ctx, log, srv)
	}

	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listener failed", "err", err)
			stop()
		}
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(sctx)
	}
	return nil
}

func listen(srv *http.Server, tlsCfg *tls.Config) error {
	if tlsCfg != nil {
		return srv.ListenAndServeTLS("", "")
	}
	return srv.ListenAndServe()
}

// listenRetry keeps trying to bind an admin address: the Tailscale interface
// may come up after the container starts.
func listenRetry(ctx context.Context, log *slog.Logger, srv *http.Server) {
	for {
		ln, err := net.Listen("tcp", srv.Addr)
		if err == nil {
			log.Info("admin listener up", "addr", srv.Addr)
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("admin listener", "addr", srv.Addr, "err", err)
			}
			return
		}
		log.Warn("admin listener not yet available; retrying", "addr", srv.Addr, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
	}
}

// adminHandler serves metrics, pprof and the unknown-endpoint report. Every
// request is re-checked against the peer address, in addition to binding
// only loopback or Tailscale addresses.
func adminHandler(m *metrics.Metrics, apiSrv *api.Server) http.Handler {
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
		if err != nil || !config.IsAdminPeer(a) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// maintenance purges idle sessions and stale uploads hourly.
func maintenance(ctx context.Context, log *slog.Logger, st *store.Store, cfg *config.Config, sessions *atomic.Int64) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := st.PurgeSessions(ctx, time.Now().Add(-cfg.SessionIdleExpiry)); err == nil && n > 0 {
			log.Info("purged idle sessions", "count", n)
		}
		st.PurgeUploads(ctx, time.Now().Add(-7*24*time.Hour))
		if n, err := st.CountSessions(ctx); err == nil {
			sessions.Store(n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
