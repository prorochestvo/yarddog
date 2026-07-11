package infrastructure

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

func TestLoadConfig(t *testing.T) {
	t.Run("all required present applies defaults", func(t *testing.T) {
		t.Parallel()

		path := writeConfigFixture(t, requiredOnlyEnv())

		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}

		if cfg.Label != "home" || cfg.TelegramBotDSN != "tbot://1:@tok/" ||
			cfg.RouterUser != "admin" || cfg.RouterPass != "secret" {
			t.Fatalf("required fields not carried through: %#v", cfg)
		}

		if cfg.RouterAddr != defaultRouterAddr {
			t.Errorf("RouterAddr = %q, want default %q", cfg.RouterAddr, defaultRouterAddr)
		}
		if cfg.DBPath != defaultDBPath {
			t.Errorf("DBPath = %q, want default %q", cfg.DBPath, defaultDBPath)
		}
		if cfg.CheckTimeout != 5*time.Second {
			t.Errorf("CheckTimeout = %v, want 5s", cfg.CheckTimeout)
		}
		if cfg.RecoveryInterval != 60*time.Second {
			t.Errorf("RecoveryInterval = %v, want 60s", cfg.RecoveryInterval)
		}
		if cfg.RecoveryTimeout != 15*time.Minute {
			t.Errorf("RecoveryTimeout = %v, want 15m", cfg.RecoveryTimeout)
		}
		if cfg.RebootCooldown != 2*time.Hour {
			t.Errorf("RebootCooldown = %v, want 2h", cfg.RebootCooldown)
		}
		if cfg.RetentionDays != 90 {
			t.Errorf("RetentionDays = %d, want 90", cfg.RetentionDays)
		}
		if cfg.RouterKind != domain.RouterKindNokia {
			t.Errorf("RouterKind = %q, want default %q", cfg.RouterKind, domain.RouterKindNokia)
		}
		if !cfg.RebootEnabled {
			t.Error("RebootEnabled = false, want true by default")
		}
		if !cfg.MetricsEnabled {
			t.Error("MetricsEnabled = false, want true by default")
		}
		wantMetricsSettings := domain.MetricsSettings{Temperature: true, Fans: true, CPU: true, Memory: true, Disk: true, Uptime: true, Network: true}
		if cfg.MetricsSettings != wantMetricsSettings {
			t.Errorf("MetricsSettings = %+v, want every collector enabled by default %+v", cfg.MetricsSettings, wantMetricsSettings)
		}
		if cfg.MetricsDiskMount != defaultMetricsDiskMount {
			t.Errorf("MetricsDiskMount = %q, want default %q", cfg.MetricsDiskMount, defaultMetricsDiskMount)
		}
		if len(cfg.PingHosts) != 0 {
			t.Errorf("PingHosts = %v, want empty by default (feature off)", cfg.PingHosts)
		}
		if cfg.PingCount != 5 {
			t.Errorf("PingCount = %d, want default 5", cfg.PingCount)
		}
		if cfg.PingTimeout != 4*time.Second {
			t.Errorf("PingTimeout = %v, want default 4s", cfg.PingTimeout)
		}

		wantIPs := []string{"1.1.1.1:443", "8.8.8.8:53"}
		if !reflect.DeepEqual(cfg.CheckIPs, wantIPs) {
			t.Errorf("CheckIPs = %v, want %v", cfg.CheckIPs, wantIPs)
		}
		wantDomains := []string{
			"https://www.google.com/generate_204",
			"https://cloudflare.com/cdn-cgi/trace",
		}
		if !reflect.DeepEqual(cfg.CheckDomains, wantDomains) {
			t.Errorf("CheckDomains = %v, want %v", cfg.CheckDomains, wantDomains)
		}
	})

	t.Run("missing required key names it in the error", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		delete(env, "ROUTER_PASS")
		path := writeConfigFixture(t, env)

		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("LoadConfig() error = nil, want error naming ROUTER_PASS")
		}
		if !strings.Contains(err.Error(), "ROUTER_PASS") {
			t.Fatalf("LoadConfig() error = %q, want it to mention ROUTER_PASS", err)
		}
	})

	t.Run("missing all required keys names all of them", func(t *testing.T) {
		t.Parallel()

		path := writeConfigFixture(t, map[string]string{})

		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("LoadConfig() error = nil, want error naming all required keys")
		}
		for _, key := range []string{"LABEL", "TELEGRAMBOT_DSN", "ROUTER_USER", "ROUTER_PASS"} {
			if !strings.Contains(err.Error(), key) {
				t.Errorf("LoadConfig() error = %q, want it to mention %s", err, key)
			}
		}
	})

	t.Run("real environment overrides the file", func(t *testing.T) {
		// t.Setenv restores the variable via Cleanup and cannot be combined
		// with t.Parallel on this test or any ancestor.
		env := requiredOnlyEnv()
		env["LABEL"] = "from-file"
		path := writeConfigFixture(t, env)

		t.Setenv(envPrefix+"LABEL", "from-real-env")

		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg.Label != "from-real-env" {
			t.Fatalf("Label = %q, want real-env value %q", cfg.Label, "from-real-env")
		}
	})

	t.Run("invalid duration returns an error", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		env["CHECK_TIMEOUT"] = "not-a-duration"
		path := writeConfigFixture(t, env)

		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("LoadConfig() error = nil, want error for invalid CHECK_TIMEOUT")
		}
	})

	t.Run("invalid retention days returns an error", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		env["RETENTION_DAYS"] = "forever"
		path := writeConfigFixture(t, env)

		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("LoadConfig() error = nil, want error for invalid RETENTION_DAYS")
		}
	})

	t.Run("explicit ROUTER_KIND is parsed onto Config", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		env["ROUTER_KIND"] = "nokia"
		path := writeConfigFixture(t, env)

		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg.RouterKind != domain.RouterKindNokia {
			t.Fatalf("RouterKind = %q, want %q", cfg.RouterKind, domain.RouterKindNokia)
		}
	})

	t.Run("invalid ROUTER_KIND returns an error", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		env["ROUTER_KIND"] = "tapo"
		path := writeConfigFixture(t, env)

		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("LoadConfig() error = nil, want error for an unsupported ROUTER_KIND")
		}
	})

	t.Run("retention days zero is valid and means keep forever", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		env["RETENTION_DAYS"] = "0"
		path := writeConfigFixture(t, env)

		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg.RetentionDays != 0 {
			t.Fatalf("RetentionDays = %d, want 0", cfg.RetentionDays)
		}
	})

	t.Run("REBOOT_ENABLED explicit false is parsed", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		env["REBOOT_ENABLED"] = "false"
		path := writeConfigFixture(t, env)

		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg.RebootEnabled {
			t.Fatal("RebootEnabled = true, want false")
		}
	})

	t.Run("METRICS_FANS false disables only the fans collector", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		env["METRICS_FANS"] = "false"
		path := writeConfigFixture(t, env)

		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg.MetricsSettings.Fans {
			t.Fatal("MetricsSettings.Fans = true, want false")
		}
		want := domain.MetricsSettings{Temperature: true, Fans: false, CPU: true, Memory: true, Disk: true, Uptime: true, Network: true}
		if cfg.MetricsSettings != want {
			t.Fatalf("MetricsSettings = %+v, want only Fans disabled %+v", cfg.MetricsSettings, want)
		}
	})

	t.Run("invalid METRICS_CPU returns an error naming it", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		env["METRICS_CPU"] = "banana"
		path := writeConfigFixture(t, env)

		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("LoadConfig() error = nil, want error for invalid METRICS_CPU")
		}
		if !strings.Contains(err.Error(), "METRICS_CPU") {
			t.Fatalf("LoadConfig() error = %q, want it to mention METRICS_CPU", err)
		}
	})

	t.Run("custom METRICS_DISK_MOUNT is carried through", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		env["METRICS_DISK_MOUNT"] = "/data"
		path := writeConfigFixture(t, env)

		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg.MetricsDiskMount != "/data" {
			t.Fatalf("MetricsDiskMount = %q, want %q", cfg.MetricsDiskMount, "/data")
		}
	})

	t.Run("check ips and domains split on comma and trimmed", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		env["CHECK_IPS"] = " 1.2.3.4:443 , 5.6.7.8:53 "
		env["CHECK_DOMAINS"] = "https://a.example/,https://b.example/"
		path := writeConfigFixture(t, env)

		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}

		wantIPs := []string{"1.2.3.4:443", "5.6.7.8:53"}
		if !reflect.DeepEqual(cfg.CheckIPs, wantIPs) {
			t.Fatalf("CheckIPs = %v, want %v", cfg.CheckIPs, wantIPs)
		}
		wantDomains := []string{"https://a.example/", "https://b.example/"}
		if !reflect.DeepEqual(cfg.CheckDomains, wantDomains) {
			t.Fatalf("CheckDomains = %v, want %v", cfg.CheckDomains, wantDomains)
		}
	})

	t.Run("PING_HOSTS splits on comma and trims", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		env["PING_HOSTS"] = " 1.1.1.1 , 8.8.8.8 "
		path := writeConfigFixture(t, env)

		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		want := []string{"1.1.1.1", "8.8.8.8"}
		if !reflect.DeepEqual(cfg.PingHosts, want) {
			t.Fatalf("PingHosts = %v, want %v", cfg.PingHosts, want)
		}
	})

	t.Run("PING_COUNT below the floor is clamped to 4", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		env["PING_COUNT"] = "3"
		path := writeConfigFixture(t, env)

		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg.PingCount != 4 {
			t.Fatalf("PingCount = %d, want clamped to 4", cfg.PingCount)
		}
	})

	t.Run("PING_COUNT above the ceiling is clamped to 7", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		env["PING_COUNT"] = "10"
		path := writeConfigFixture(t, env)

		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg.PingCount != 7 {
			t.Fatalf("PingCount = %d, want clamped to 7", cfg.PingCount)
		}
	})

	t.Run("non-integer PING_COUNT returns an error", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		env["PING_COUNT"] = "many"
		path := writeConfigFixture(t, env)

		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("LoadConfig() error = nil, want error for non-integer PING_COUNT")
		}
	})

	t.Run("sub-second PING_TIMEOUT is clamped up to 1s", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		env["PING_TIMEOUT"] = "200ms"
		path := writeConfigFixture(t, env)

		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg.PingTimeout != time.Second {
			t.Fatalf("PingTimeout = %v, want clamped up to 1s", cfg.PingTimeout)
		}
	})

	t.Run("PING_TIMEOUT above the ceiling is clamped to 10s", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		env["PING_TIMEOUT"] = "30s"
		path := writeConfigFixture(t, env)

		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg.PingTimeout != 10*time.Second {
			t.Fatalf("PingTimeout = %v, want clamped down to 10s (below the collectPings batch backstop)", cfg.PingTimeout)
		}
	})

	t.Run("a PING_HOSTS entry beginning with \"-\" is rejected (argument-injection guard)", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		env["PING_HOSTS"] = "1.1.1.1,-evil.com"
		path := writeConfigFixture(t, env)

		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("LoadConfig() error = nil, want error for a PING_HOSTS entry beginning with \"-\"")
		}
		if !strings.Contains(err.Error(), "PING_HOSTS") {
			t.Fatalf("LoadConfig() error = %q, want it to mention PING_HOSTS", err)
		}
	})

	t.Run("invalid PING_TIMEOUT returns an error", func(t *testing.T) {
		t.Parallel()

		env := requiredOnlyEnv()
		env["PING_TIMEOUT"] = "not-a-duration"
		path := writeConfigFixture(t, env)

		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("LoadConfig() error = nil, want error for invalid PING_TIMEOUT")
		}
	})

	t.Run("missing env file still succeeds when real environment supplies required keys", func(t *testing.T) {
		env := requiredOnlyEnv()
		for k, v := range env {
			t.Setenv(envPrefix+k, v)
		}

		cfg, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.env"))
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg.Label != "home" {
			t.Fatalf("Label = %q, want %q", cfg.Label, "home")
		}
	})
}

// requiredOnlyEnv returns a fresh map holding exactly the four required keys,
// so each subtest can mutate its own copy without affecting others.
func requiredOnlyEnv() map[string]string {
	return map[string]string{
		"LABEL":           "home",
		"TELEGRAMBOT_DSN": "tbot://1:@tok/",
		"ROUTER_USER":     "admin",
		"ROUTER_PASS":     "secret",
	}
}

// writeConfigFixture renders env as KEY=VALUE lines into a fresh file under
// t.TempDir() and returns its path, so tests never touch a real .env. Callers
// pass the bare logical keys (LABEL, DAEMON_TOKEN, …); the fixture writes them
// under the envPrefix namespace the loaders read (YARDDOG_LABEL, …), so a test
// exercises the same key names an operator's real env file uses.
func writeConfigFixture(t *testing.T, env map[string]string) string {
	t.Helper()

	var b strings.Builder
	for k, v := range env {
		b.WriteString(envPrefix + k)
		b.WriteByte('=')
		b.WriteString(v)
		b.WriteByte('\n')
	}

	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}

	return path
}
