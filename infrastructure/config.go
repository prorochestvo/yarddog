package infrastructure

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

// LoadConfig builds a Config by reading envPath (a KEY=VALUE file, see
// loadEnvFile) and merging it with the real process environment, where a real
// environment variable always wins over the same key in the file. Optional
// keys fall back to the design's documented defaults; the four required keys
// (LABEL, TELEGRAMBOT_DSN, ROUTER_USER, ROUTER_PASS) must resolve to a
// non-empty value or LoadConfig returns an error naming every missing one.
func LoadConfig(envPath string) (*Config, error) {
	get, err := newEnvLookup(envPath)
	if err != nil {
		return nil, err
	}

	var missing []string
	required := func(key string) string {
		v, ok := get(key)
		if !ok || v == "" {
			missing = append(missing, envPrefix+key)
		}
		return v
	}

	cfg := &Config{
		Label:          required("LABEL"),
		TelegramBotDSN: required("TELEGRAMBOT_DSN"),
		RouterUser:     required("ROUTER_USER"),
		RouterPass:     required("ROUTER_PASS"),
		RouterAddr:     getOrDefault(get, "ROUTER_ADDR", defaultRouterAddr),
		DBPath:         getOrDefault(get, "DB_PATH", defaultDBPath),
		CheckIPs:       splitCSV(getOrDefault(get, "CHECK_IPS", defaultCheckIPs)),
		CheckDomains:   splitCSV(getOrDefault(get, "CHECK_DOMAINS", defaultCheckDomains)),
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required config value(s): %s", strings.Join(missing, ", "))
	}

	routerKind, err := domain.ParseRouterKind(getOrDefault(get, "ROUTER_KIND", ""))
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", envPrefix+"ROUTER_KIND", err)
	}
	cfg.RouterKind = routerKind

	durations := []struct {
		key string
		dst *time.Duration
		def string
	}{
		{"CHECK_TIMEOUT", &cfg.CheckTimeout, defaultCheckTimeout},
		{"RECOVERY_INTERVAL", &cfg.RecoveryInterval, defaultRecoveryInterval},
		{"RECOVERY_TIMEOUT", &cfg.RecoveryTimeout, defaultRecoveryTimeout},
		{"REBOOT_COOLDOWN", &cfg.RebootCooldown, defaultRebootCooldown},
	}
	for _, d := range durations {
		raw := getOrDefault(get, d.key, d.def)
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid %s %q: %w", envPrefix+d.key, raw, err)
		}
		*d.dst = parsed
	}

	rawRetention := getOrDefault(get, "RETENTION_DAYS", defaultRetentionDays)
	retention, err := strconv.Atoi(rawRetention)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", envPrefix+"RETENTION_DAYS", rawRetention, err)
	}
	cfg.RetentionDays = retention

	bools := []struct {
		key string
		dst *bool
		def bool
	}{
		{"REBOOT_ENABLED", &cfg.RebootEnabled, true},
		{"METRICS_ENABLED", &cfg.MetricsEnabled, true},
		{"METRICS_TEMPERATURE", &cfg.MetricsSettings.Temperature, true},
		{"METRICS_FANS", &cfg.MetricsSettings.Fans, true},
		{"METRICS_CPU", &cfg.MetricsSettings.CPU, true},
		{"METRICS_MEMORY", &cfg.MetricsSettings.Memory, true},
		{"METRICS_DISK", &cfg.MetricsSettings.Disk, true},
		{"METRICS_UPTIME", &cfg.MetricsSettings.Uptime, true},
		{"METRICS_NETWORK", &cfg.MetricsSettings.Network, true},
	}
	for _, b := range bools {
		v, err := getBool(get, b.key, b.def)
		if err != nil {
			return nil, err
		}
		*b.dst = v
	}

	cfg.MetricsDiskMount = getOrDefault(get, "METRICS_DISK_MOUNT", defaultMetricsDiskMount)

	cfg.PingHosts = splitCSV(getOrDefault(get, "PING_HOSTS", defaultPingHosts))
	for _, host := range cfg.PingHosts {
		if strings.HasPrefix(host, "-") {
			return nil, fmt.Errorf("invalid %s: host %q must not begin with \"-\" (argument-injection guard)", envPrefix+"PING_HOSTS", host)
		}
	}

	rawPingCount := getOrDefault(get, "PING_COUNT", defaultPingCount)
	pingCount, err := strconv.Atoi(rawPingCount)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", envPrefix+"PING_COUNT", rawPingCount, err)
	}
	cfg.PingCount = clampPingCount(pingCount)

	rawPingTimeout := getOrDefault(get, "PING_TIMEOUT", defaultPingTimeout)
	pingTimeout, err := time.ParseDuration(rawPingTimeout)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", envPrefix+"PING_TIMEOUT", rawPingTimeout, err)
	}
	cfg.PingTimeout = clampPingTimeout(pingTimeout)

	return cfg, nil
}

// Config holds the fully resolved, validated configuration for a single run.
type Config struct {
	Label            string
	TelegramBotDSN   string
	RouterKind       domain.RouterKind
	RouterAddr       string
	RouterUser       string
	RouterPass       string
	DBPath           string
	CheckIPs         []string
	CheckDomains     []string
	CheckTimeout     time.Duration
	RecoveryInterval time.Duration
	RecoveryTimeout  time.Duration
	RebootCooldown   time.Duration
	RetentionDays    int
	RebootEnabled    bool
	MetricsEnabled   bool
	MetricsSettings  domain.MetricsSettings
	MetricsDiskMount string
	PingHosts        []string
	PingCount        int
	PingTimeout      time.Duration
}

const (
	defaultRouterAddr       = "http://192.168.1.1"
	defaultDBPath           = "/var/lib/yarddog/yarddog.db"
	defaultCheckIPs         = "1.1.1.1:443,8.8.8.8:53"
	defaultCheckDomains     = "https://www.google.com/generate_204,https://cloudflare.com/cdn-cgi/trace"
	defaultCheckTimeout     = "5s"
	defaultRecoveryInterval = "60s"
	defaultRecoveryTimeout  = "15m"
	defaultRebootCooldown   = "2h"
	defaultRetentionDays    = "90"
	defaultMetricsDiskMount = "/"
	defaultPingHosts        = ""
	defaultPingCount        = "5"
	defaultPingTimeout      = "4s"

	// minPingCount and maxPingCount bound PING_COUNT (issue #2): too few
	// probes make the average noisy, too many turn a per-run collector into
	// a multi-second network burst against every configured host.
	minPingCount = 4
	maxPingCount = 7

	// minPingTimeout and maxPingTimeout bound PING_TIMEOUT (issue #2). The
	// per-host deadline is passed to ping as an integer-second -w/-t flag, so
	// a sub-second value must floor to 1s or ping runs with no deadline at
	// all. The ceiling keeps the per-host deadline safely nested inside the
	// orchestrator's whole-batch collectPings backstop (defaultPingTimeout,
	// 15s): a value above it would be silently killed by the batch deadline
	// every run, making the operator's setting a no-op.
	minPingTimeout = 1 * time.Second
	maxPingTimeout = 10 * time.Second
)

// clampPingCount bounds PING_COUNT to [minPingCount, maxPingCount], silently
// correcting an out-of-range operator value rather than failing startup over
// it (matching QueryService.clampLimit's "clamp, don't error" philosophy for
// a value that is worth correcting, not worth refusing to start over).
func clampPingCount(n int) int {
	if n < minPingCount {
		return minPingCount
	}
	if n > maxPingCount {
		return maxPingCount
	}
	return n
}

// clampPingTimeout bounds PING_TIMEOUT to [minPingTimeout, maxPingTimeout],
// silently correcting an out-of-range value rather than refusing to start
// (matching clampPingCount's "clamp, don't error" philosophy).
func clampPingTimeout(d time.Duration) time.Duration {
	if d < minPingTimeout {
		return minPingTimeout
	}
	if d > maxPingTimeout {
		return maxPingTimeout
	}
	return d
}

// getBool looks up key via get and parses it as a bool (strconv.ParseBool:
// "true/false/1/0/t/f", case-insensitive), or returns def if the key
// resolved to nothing (missing or empty — matching getOrDefault's "empty ==
// unset" rule so e.g. REBOOT_ENABLED= does not blank the default to false).
func getBool(get func(string) (string, bool), key string, def bool) (bool, error) {
	raw, ok := get(key)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("invalid %s %q: %w", envPrefix+key, raw, err)
	}
	return v, nil
}

// getOrDefault looks up key via get and returns its value, or def if the key
// resolved to nothing (missing or empty — an empty override is treated the
// same as unset so `FOO=` in a file doesn't silently blank out a default).
func getOrDefault(get func(string) (string, bool), key, def string) string {
	if v, ok := get(key); ok && v != "" {
		return v
	}
	return def
}

// envPrefix namespaces every yarddog configuration key in both the process
// environment and the env file (YARDDOG_LABEL, YARDDOG_DB_PATH,
// YARDDOG_DAEMON_TOKEN, …), so a generic name like LABEL or DB_PATH never
// collides with an unrelated variable already present in the environment. It
// is applied in exactly one place — newEnvLookup's get closure — so every
// call site in LoadConfig/LoadDaemonConfig keeps naming the bare logical key,
// and only operator-facing error messages re-attach the prefix so they name
// the real variable.
const envPrefix = "YARDDOG_"

// newEnvLookup builds the "real environment wins over the file" lookup
// closure LoadConfig and LoadDaemonConfig both use: it reads envPath once
// via loadEnvFile, then returns a get(key) that checks the real process
// environment first and falls back to the file's value. The caller names the
// bare logical key (e.g. "LABEL"); the closure prepends envPrefix before
// touching either source, so both the environment and the file are read under
// the namespaced name (YARDDOG_LABEL).
func newEnvLookup(envPath string) (func(string) (string, bool), error) {
	fileValues, err := loadEnvFile(envPath)
	if err != nil {
		return nil, err
	}

	return func(key string) (string, bool) {
		key = envPrefix + key
		if v, ok := os.LookupEnv(key); ok {
			return v, true
		}
		v, ok := fileValues[key]
		return v, ok
	}, nil
}

// splitCSV splits s on commas, trims surrounding space from each piece, and
// drops empty pieces (e.g. from a trailing comma) so callers never see a
// spurious empty target.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}
