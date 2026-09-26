// Command mockingbird is a bridge that lets 2009-era Twitter clients use
// Bluesky, by serving the Twitter REST API v1 and Search API and translating
// each request into AT Protocol calls against the user's own PDS.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/j4ckxyz/mockingbird/internal/app"
	"github.com/j4ckxyz/mockingbird/internal/config"
	"github.com/j4ckxyz/mockingbird/internal/loadtest"
	"github.com/j4ckxyz/mockingbird/internal/logging"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

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
		case "loadtest":
			os.Exit(loadtest.Main(os.Args[2:]))
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
	a, err := app.New(cfg, app.Options{Logger: log})
	if err != nil {
		return err
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go a.Maintain(ctx)

	errLog := slog.NewLogLogger(log.Handler(), slog.LevelWarn)
	newServer := func(addr string, h http.Handler) *http.Server {
		return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
			WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 32 << 10, ErrorLog: errLog}
	}
	var servers []*http.Server
	errc := make(chan error, 4)

	httpSrv := newServer(cfg.HTTPAddr, a.Handler)
	servers = append(servers, httpSrv)
	go func() { errc <- httpSrv.ListenAndServe() }()

	if a.TLS != nil {
		// The legacy listener is its own server with its own TLS config, and
		// serves only the public handler. HTTP/2 is off: these clients speak 1.1.
		tlsSrv := newServer(cfg.LegacyTLSAddr, a.Handler)
		tlsSrv.TLSConfig = a.TLS
		tlsSrv.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
		servers = append(servers, tlsSrv)
		go func() { errc <- tlsSrv.ListenAndServeTLS("", "") }()
		log.Info("legacy TLS listener enabled", "addr", cfg.LegacyTLSAddr, "cn", cfg.TLSCommonName)
	}

	for _, addr := range cfg.AdminAddrs {
		srv := newServer(addr, a.Admin)
		servers = append(servers, srv)
		go listenRetry(ctx, log, srv)
	}

	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listener failed", "err", err)
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
