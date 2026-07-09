package infrastructure

import (
	"fmt"
	"strconv"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

// LoadDaemonConfig builds a DaemonConfig by reading envPath (see
// newEnvLookup) and merging it with the real process environment, exactly
// like LoadConfig. DAEMON_TOKEN is the sole required key, so the daemon
// never fails to start over a missing router password or Telegram DSN, and
// it never uses or emits them. It does not scrub those keys from memory:
// the shared env file is parsed whole and systemd's EnvironmentFile places
// every value in the process environment anyway, so co-locating the
// collector's secrets is a deployment property, not one this loader
// controls.
func LoadDaemonConfig(envPath string) (*DaemonConfig, error) {
	get, err := newEnvLookup(envPath)
	if err != nil {
		return nil, err
	}

	token, ok := get("DAEMON_TOKEN")
	if !ok || token == "" {
		return nil, fmt.Errorf("missing required config value: %s", envPrefix+"DAEMON_TOKEN")
	}

	cfg := &DaemonConfig{
		Token:  token,
		Addr:   getOrDefault(get, "DAEMON_ADDR", defaultDaemonAddr),
		DBPath: getOrDefault(get, "DB_PATH", defaultDBPath),
	}

	durations := []struct {
		key string
		dst *time.Duration
		def string
	}{
		{"DAEMON_STALE_AFTER", &cfg.StaleAfter, defaultStaleAfter},
		{"DAEMON_READ_TIMEOUT", &cfg.ReadTimeout, defaultDaemonReadTimeout},
		{"DAEMON_WRITE_TIMEOUT", &cfg.WriteTimeout, defaultDaemonWriteTimeout},
		{"DAEMON_IDLE_TIMEOUT", &cfg.IdleTimeout, defaultDaemonIdleTimeout},
		{"DAEMON_HEALTH_TIMEOUT", &cfg.HealthTimeout, defaultHealthTimeout},
	}
	for _, d := range durations {
		raw := getOrDefault(get, d.key, d.def)
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid %s %q: %w", envPrefix+d.key, raw, err)
		}
		*d.dst = parsed
	}

	rawMaxConns := getOrDefault(get, "DAEMON_MAX_CONNS", defaultDaemonMaxConns)
	maxConns, err := strconv.Atoi(rawMaxConns)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", envPrefix+"DAEMON_MAX_CONNS", rawMaxConns, err)
	}
	cfg.MaxConns = maxConns

	rh, err := loadRouterHealthConfig(get)
	if err != nil {
		return nil, err
	}
	cfg.RouterHealth = rh

	if rh != nil {
		if rh.HTTPTimeout <= 0 || rh.HTTPTimeout >= cfg.HealthTimeout {
			return nil, fmt.Errorf(
				"%sCHECK_TIMEOUT (%s) must be > 0 and < %sDAEMON_HEALTH_TIMEOUT (%s)",
				envPrefix, rh.HTTPTimeout, envPrefix, cfg.HealthTimeout,
			)
		}
	}

	return cfg, nil
}

// loadRouterHealthConfig reads the optional router-probe keys from get and
// returns a non-nil *DaemonRouterHealth only when both ROUTER_USER and
// ROUTER_PASS are present. Exactly one present => error (half-configured is an
// operator mistake, not a silent no-op). Neither present => nil, no error.
func loadRouterHealthConfig(get func(string) (string, bool)) (*DaemonRouterHealth, error) {
	user, userOK := get("ROUTER_USER")
	pass, passOK := get("ROUTER_PASS")
	userSet := userOK && user != ""
	passSet := passOK && pass != ""

	if userSet && !passSet {
		return nil, fmt.Errorf("router health probe partially configured: %sROUTER_USER set but %sROUTER_PASS missing (set both to enable, neither to disable)", envPrefix, envPrefix)
	}
	if passSet && !userSet {
		return nil, fmt.Errorf("router health probe partially configured: %sROUTER_PASS set but %sROUTER_USER missing (set both to enable, neither to disable)", envPrefix, envPrefix)
	}
	if !userSet {
		// neither set — probe disabled, daemon starts fine on DAEMON_TOKEN alone.
		return nil, nil
	}

	rawKind := getOrDefault(get, "ROUTER_KIND", "")
	kind, err := domain.ParseRouterKind(rawKind)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", envPrefix+"ROUTER_KIND", err)
	}

	rawTimeout := getOrDefault(get, "CHECK_TIMEOUT", defaultRouterHTTPTimeout)
	httpTimeout, err := time.ParseDuration(rawTimeout)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", envPrefix+"CHECK_TIMEOUT", rawTimeout, err)
	}

	return &DaemonRouterHealth{
		Kind:        kind,
		Addr:        getOrDefault(get, "ROUTER_ADDR", defaultRouterAddr),
		User:        user,
		Pass:        pass,
		ProbeName:   getOrDefault(get, "DAEMON_HEALTH_ROUTER", defaultRouterProbeName),
		HTTPTimeout: httpTimeout,
	}, nil
}

// DaemonConfig holds the fully resolved, validated configuration for the
// yarddogd query daemon (cmd/yarddogd) — a separate, narrower surface than
// the collector's Config (see LoadDaemonConfig).
type DaemonConfig struct {
	Token         string
	Addr          string
	DBPath        string
	StaleAfter    time.Duration
	ReadTimeout   time.Duration
	WriteTimeout  time.Duration
	IdleTimeout   time.Duration
	HealthTimeout time.Duration
	MaxConns      int
	// RouterHealth is non-nil only when both YARDDOG_ROUTER_USER and
	// YARDDOG_ROUTER_PASS are set. nil means the router credential probe is
	// disabled and the daemon starts on DAEMON_TOKEN alone.
	RouterHealth *DaemonRouterHealth
}

// DaemonRouterHealth is the resolved, validated router-probe config; it is
// non-nil only when both ROUTER_USER and ROUTER_PASS are supplied.
// The nil-pointer form makes "probe disabled" and "half-configured" both
// unrepresentable as valid config — half-configured is an error at load time.
type DaemonRouterHealth struct {
	Kind        domain.RouterKind
	Addr        string
	User        string
	Pass        string
	ProbeName   string
	HTTPTimeout time.Duration
}

// defaultDaemonAddr is intentionally loopback-only: a misconfigured daemon
// binds locally instead of exposing the LAN by accident. Production sets
// DAEMON_ADDR to a LAN IP:port explicitly (README "Daemon / query API").
const (
	defaultDaemonAddr         = "127.0.0.1:8420"
	defaultStaleAfter         = "90m"
	defaultDaemonReadTimeout  = "10s"
	defaultDaemonWriteTimeout = "15s"
	defaultDaemonIdleTimeout  = "60s"
	defaultHealthTimeout      = "5s"
	// 2, not 1: the daemon's DB is a file-backed WAL SQLite, where concurrent
	// readers are safe (unlike the :memory: single-connection restriction
	// elsewhere in this codebase), so the second connection gives
	// /health/check's sqlite probe headroom to run without queuing behind an
	// in-flight data read on the only connection — a busy-not-broken daemon
	// would otherwise falsely report 503.
	defaultDaemonMaxConns = "2"
	// defaultRouterProbeName is the /health/check report key for the router
	// credential probe when YARDDOG_DAEMON_HEALTH_ROUTER is not set. Operators
	// typically override it with their device's network name (e.g. "gipon").
	defaultRouterProbeName = "router"
	// defaultRouterHTTPTimeout is the per-request login timeout for the router
	// credential probe. It is intentionally shorter than defaultHealthTimeout
	// (5s) so a wedged router login never burns the whole health-sweep budget —
	// the probe must time out before the Inspector's deadline fires.
	defaultRouterHTTPTimeout = "3s"
)
