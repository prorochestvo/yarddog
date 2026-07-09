// Command yarddog is a home-network watchdog for a GPON router: a short-lived
// process, fired by cron on a Raspberry Pi, that checks the internet uplink and
// reboots the router over its web UI when the link is down.
//
// Usage:
//
//	yarddog                        # soft mode: reboot only if the internet is down
//	yarddog --hard-reboot          # hard mode: reboot unconditionally
//	yarddog --env /path/to/file    # env file path (default /opt/yarddog/.env, falling
//	                                # back to ./.env if that file does not exist)
//
// Exit codes (design §2): 0 ok, 1 config error, 2 another instance already
// running, 3 reboot request failed, 4 reboot done but internet not restored
// within the timeout, 5 reboot skipped due to cooldown.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/prorochestvo/yarddog/domain"
	"github.com/prorochestvo/yarddog/gateway/router"
	"github.com/prorochestvo/yarddog/gateway/telegram"
	"github.com/prorochestvo/yarddog/infrastructure"
	"github.com/prorochestvo/yarddog/services"
)

func main() {
	os.Exit(run())
}

// run does the real work of main and returns the process exit code instead of
// calling os.Exit directly, so every defer (the store Close included) fires
// before main hands the code to os.Exit. main is the only place that calls
// os.Exit (CLAUDE.md Error Handling); every terminal exit code (0/3/4/5) is
// decided by services.Execute, and run here only ever produces
// domain.ExitConfigError or domain.ExitLockHeld for failures that happen
// before Execute can be reached — main is the composition root: it is the
// only package that imports every layer.
func run() int {
	hardReboot := flag.Bool("hard-reboot", false, "reboot the router unconditionally instead of only when the internet is down")
	envPath := flag.String("env", "", "path to the env file (default /opt/yarddog/.env, falling back to ./.env)")
	flag.Parse()

	cfg, err := infrastructure.LoadConfig(resolveEnvPath(*envPath))
	if err != nil {
		log.Printf("yarddog: config error: %v", err)
		return domain.ExitConfigError
	}

	// ensure the DB directory exists before it hosts either the flock or the
	// store: the lock file (lockPathFor) and the SQLite database share this
	// directory, and neither os.OpenFile (for the lock) nor the sqlite driver
	// creates a missing parent. Without this a first run on an unprovisioned
	// path would fail opening the lock and be misreported as ExitLockHeld
	// ("another instance running?") rather than the config error it is.
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o700); err != nil {
		log.Printf("yarddog: create data dir: %v", err)
		return domain.ExitConfigError
	}

	// acquire the lock before opening the store — the shared SQLite database is
	// the resource concurrent runs must not race on, and the lock lives in that
	// database's own directory (lockPathFor). LoadConfig above only reads the
	// env (it touches no shared state), so resolving the DB path first, then
	// locking, keeps the lock ahead of every write exactly as before.
	lockFile, err := acquireLock(lockPathFor(cfg.DBPath))
	if err != nil {
		log.Printf("yarddog: %v", err)
		return domain.ExitLockHeld
	}
	// keep lockFile reachable for the rest of run so the GC never finalizes
	// (and thereby closes, releasing the flock) the *os.File while the up-to-
	// 15m recovery loop is still using the lock it holds; the lock itself is
	// released only by process exit, never by an explicit Close here.
	defer runtime.KeepAlive(lockFile)

	ctx := context.Background()

	st, err := infrastructure.NewStore(ctx, cfg.DBPath)
	if err != nil {
		log.Printf("yarddog: open store: %v", err)
		return domain.ExitConfigError
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Printf("yarddog: close store: %v", err)
		}
	}()

	chk, rb, nt, mc, err := wire(cfg, st)
	if err != nil {
		log.Printf("yarddog: %v", err)
		return domain.ExitConfigError
	}

	clk := infrastructure.SystemClock{}
	if err := st.PruneChecks(ctx, clk.Now(), cfg.RetentionDays); err != nil {
		log.Printf("yarddog: prune checks: %v", err)
	}
	if err := st.PruneMetrics(ctx, clk.Now(), cfg.RetentionDays); err != nil {
		log.Printf("yarddog: prune metrics: %v", err)
	}
	if err := nt.Flush(ctx); err != nil {
		log.Printf("yarddog: startup outbox flush: %v", err)
	}

	mode := domain.ModeSoft
	if *hardReboot {
		mode = domain.ModeHard
	}

	settings := services.Settings{
		Label:            cfg.Label,
		RebootCooldown:   cfg.RebootCooldown,
		RecoveryInterval: cfg.RecoveryInterval,
		RecoveryTimeout:  cfg.RecoveryTimeout,
		RebootEnabled:    cfg.RebootEnabled,
		MetricsEnabled:   cfg.MetricsEnabled,
	}

	return services.Execute(ctx, settings, st, st, chk, rb, nt, mc, clk, mode)
}

// defaultEnvPath and fallbackEnvPath are config-independent constants: the
// env-path fallback has to be resolved before LoadConfig exists to read it.
const (
	defaultEnvPath  = "/opt/yarddog/.env"
	fallbackEnvPath = "./.env"
)

// lockFileName is the flock's filename; acquireLock places it in the
// configured database's own directory (lockPathFor), so the lock is
// self-contained beside the data it serializes access to and needs no
// separate provisioning.
const lockFileName = "yarddog.lock"

// acquireLock opens (creating if absent) an exclusive, non-blocking flock on
// path and returns the open file holding it. The kernel releases the lock
// when the process dies for any reason, so there is no stale-lock case to
// guard against — do not add PID/liveness bookkeeping on top of this
// (design §10.1). Both "another instance already holds the lock" and "the
// lock file itself could not be opened" (e.g. the DB directory is not
// writable) map to the same ExitLockHeld in the caller: either way the process
// cannot safely proceed, and a distinct exit code would not change what an
// operator does about it. run creates the DB directory before calling this, so
// a merely-missing directory is not one of those failures.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lockErr := fmt.Errorf("lock %s (another instance running?): %w", path, err)
		if closeErr := f.Close(); closeErr != nil {
			return nil, fmt.Errorf("%w (also failed to close lock file: %v)", lockErr, closeErr)
		}
		return nil, lockErr
	}

	return f, nil
}

// lockPathFor returns the flock path for a given DB path: lockFileName in the
// database's own directory, so the lock lives wherever the data lives
// (/opt/yarddog/yarddog.lock beside /opt/yarddog/yarddog.sqlite, ./yarddog.lock
// beside a local ./yarddog.db) and no separate provisioning under /var/run is
// needed. It is resolved from the configured DB path, so it must be called
// after LoadConfig.
func lockPathFor(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), lockFileName)
}

// resolveEnvPath returns flagPath if the caller passed --env explicitly.
// Otherwise it tries defaultEnvPath and falls back to fallbackEnvPath if that
// file does not exist (design §2/§3). This has to happen here rather than
// inside LoadConfig: loadEnvFile treats any missing file as "config comes
// entirely from the real environment" and returns no error, so main is the
// only layer that can tell "explicitly missing, fall back" apart from
// "present but unreadable, surface the error".
func resolveEnvPath(flagPath string) string {
	if flagPath != "" {
		return flagPath
	}
	if _, err := os.Stat(defaultEnvPath); err == nil {
		return defaultEnvPath
	}
	return fallbackEnvPath
}

// wire constructs the real checker/rebooter/notifier/metrics-collector
// implementations services.Execute needs. Any failure here (a malformed
// DSN, router address, or ROUTER_KIND) is a configuration problem
// discovered only once cfg's fields are actually parsed by their consumers,
// so the caller treats it exactly like a LoadConfig failure. It is not a
// shared bootstrap package — just a private helper inside this composition
// root (CLAUDE.md: a second binary would inline its own startup, not
// import this one).
func wire(cfg *infrastructure.Config, st *infrastructure.Store) (*infrastructure.Checker, services.Rebooter, *services.OutboxService, services.MetricsCollector, error) {
	chk, err := infrastructure.NewChecker(cfg.CheckIPs, cfg.CheckDomains, cfg.RouterAddr, cfg.CheckTimeout)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("build checker: %w", err)
	}

	rb, err := router.New(cfg.RouterKind, router.RouterConfig{
		Addr:    cfg.RouterAddr,
		User:    cfg.RouterUser,
		Pass:    cfg.RouterPass,
		Timeout: cfg.CheckTimeout,
	})
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("build router driver: %w", err)
	}

	client, err := telegram.NewClient(cfg.TelegramBotDSN)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("build telegram client: %w", err)
	}

	nt := services.NewOutboxService(st, client)
	mc := infrastructure.NewMetricsCollector(cfg.MetricsSettings, cfg.MetricsDiskMount)

	return chk, rb, nt, mc, nil
}
