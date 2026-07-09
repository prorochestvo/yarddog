// Command yarddogd is yarddog's query daemon: a long-running process that
// serves the data cmd/yarddog records (runs, checks, and host telemetry) as
// a read-only JSON REST API over the LAN. It shares the collector's SQLite
// database (WAL mode; the collector is the sole writer) and every layered
// package (domain, services, infrastructure, gateway/httpapi) but has its
// own composition root and its own, disjoint required config — it never
// reads the router password or the Telegram DSN, and never fails to start
// over their absence.
//
// Security model: every route but GET /ping requires the shared secret in
// the Authorization: Bearer <token> header (RFC 6750 format, static LAN
// secret, not OAuth-issued — see gateway/httpapi). There is no TLS, no
// per-user auth, no rate limiting: DAEMON_ADDR defaults to loopback, and
// production is expected to bind a LAN address behind a firewall, not the
// open internet.
//
// Usage:
//
//	yarddogd                        # bind DAEMON_ADDR, serve until SIGINT/SIGTERM
//	yarddogd --env /path/to/file    # env file path (default /opt/yarddog/.env, falling
//	                                 # back to ./.env if that file does not exist)
//
// Exit codes — its own contract, distinct from the collector's 0-5 (design
// Trade-off T7): 0 clean shutdown, 1 configuration error, 2 the HTTP
// listener failed to start (e.g. the address is already in use).
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prorochestvo/yarddog/domain"
	"github.com/prorochestvo/yarddog/gateway/httpapi"
	"github.com/prorochestvo/yarddog/gateway/router"
	"github.com/prorochestvo/yarddog/infrastructure"
	"github.com/prorochestvo/yarddog/services"
)

// version identifies this build in /health/check's server.version field
// (gateway/dto.HealthServer). The release pipeline overrides the "dev"
// default via -ldflags "-X main.version=$VERSION_ID"; a local `go build`
// leaves it at "dev".
var version = "dev"

func main() {
	os.Exit(run())
}

// run does the real work of main and returns the process exit code instead
// of calling os.Exit directly, so every defer (the store Close included)
// fires before main hands the code to os.Exit — main is the only place that
// calls os.Exit, mirroring cmd/yarddog's own composition root.
func run() int {
	envPath := flag.String("env", "", "path to the env file (default /opt/yarddog/.env, falling back to ./.env)")
	flag.Parse()

	cfg, err := infrastructure.LoadDaemonConfig(resolveEnvPath(*envPath))
	if err != nil {
		log.Printf("yarddogd: config error: %v", err)
		return domain.ExitConfigError
	}

	// signal.NotifyContext is the whole shutdown trigger (no flock: the
	// daemon's single-instance guarantee is the port bind itself, which
	// fails outright as a listen error if a second instance starts).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := infrastructure.OpenStore(ctx, cfg.DBPath, cfg.MaxConns)
	if err != nil {
		log.Printf("yarddogd: open store: %v", err)
		return domain.ExitConfigError
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Printf("yarddogd: close store: %v", err)
		}
	}()

	clk := infrastructure.SystemClock{}

	// probes start with the two permanent ones; the router probe appended last
	// so a wedged router login cannot starve sqlite or freshness checks (the
	// inspector shares one whole-sweep deadline across all probes sequentially).
	probes := []services.HealthProbe{
		st,
		infrastructure.NewFreshnessProbe(st, clk, cfg.StaleAfter),
	}
	if rh := cfg.RouterHealth; rh != nil {
		cred, err := router.NewCredentialer(rh.Kind, router.RouterConfig{
			Addr:    rh.Addr,
			User:    rh.User,
			Pass:    rh.Pass,
			Timeout: rh.HTTPTimeout,
		})
		if err != nil {
			log.Printf("yarddogd: router health probe: %v", err)
			return domain.ExitConfigError
		}
		probes = append(probes, router.NewCredentialProbe(rh.ProbeName, cred))
	}

	insp := services.NewInspector(cfg.HealthTimeout, probes...)
	srv := httpapi.NewServer(services.NewQueryService(st), insp, cfg.Token, version, clk.Now())

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ReadHeaderTimeout: cfg.ReadTimeout,
	}

	return serve(ctx, httpServer)
}

// defaultEnvPath and fallbackEnvPath are config-independent constants,
// copied from cmd/yarddog rather than shared (CLAUDE.md: a second binary
// gives it its own composition root; the two mains do not import each
// other or a shared bootstrap package). See resolveEnvPath.
const (
	defaultEnvPath  = "/opt/yarddog/.env"
	fallbackEnvPath = "./.env"
)

// exitListenError is the daemon's own exit code (2) for "the HTTP listener
// failed to start, or died with something other than a clean shutdown" — a
// distinct, disjoint contract from the collector's 0-5 exit codes (design
// Trade-off T7).
const exitListenError = 2

// shutdownTimeout bounds serve's graceful drain once ctx is cancelled.
const shutdownTimeout = 10 * time.Second

// resolveEnvPath returns flagPath if the caller passed --env explicitly.
// Otherwise it tries defaultEnvPath and falls back to fallbackEnvPath if
// that file does not exist — copied verbatim from cmd/yarddog's own helper
// (trivial, per-binary; CLAUDE.md forbids a shared bootstrap for this).
func resolveEnvPath(flagPath string) string {
	if flagPath != "" {
		return flagPath
	}
	if _, err := os.Stat(defaultEnvPath); err == nil {
		return defaultEnvPath
	}
	return fallbackEnvPath
}

// serve starts httpServer.ListenAndServe in its own goroutine and blocks
// until either it fails (any error but the clean-shutdown sentinel
// http.ErrServerClosed, e.g. the address is already in use) or ctx is
// cancelled (SIGINT/SIGTERM). On cancellation it drains in-flight requests
// via Shutdown under its own bounded deadline, so one stuck request can
// never hang termination indefinitely — systemd's own TimeoutStopSec is the
// last-resort backstop on top of this.
func serve(ctx context.Context, httpServer *http.Server) int {
	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("yarddogd: listen: %v", err)
			return exitListenError
		}
		return domain.ExitOK

	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("yarddogd: shutdown: %v", err)
		}
		return domain.ExitOK
	}
}
