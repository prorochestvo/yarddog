package infrastructure

import (
	"strings"
	"testing"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

func TestLoadDaemonConfig(t *testing.T) {
	t.Run("missing DAEMON_TOKEN names it in the error", func(t *testing.T) {
		t.Parallel()

		path := writeConfigFixture(t, map[string]string{})

		_, err := LoadDaemonConfig(path)
		if err == nil {
			t.Fatal("LoadDaemonConfig() error = nil, want error naming DAEMON_TOKEN")
		}
		if !strings.Contains(err.Error(), "DAEMON_TOKEN") {
			t.Fatalf("LoadDaemonConfig() error = %q, want it to mention DAEMON_TOKEN", err)
		}
	})

	t.Run("empty DAEMON_TOKEN is treated as missing", func(t *testing.T) {
		t.Parallel()

		path := writeConfigFixture(t, map[string]string{"DAEMON_TOKEN": ""})

		_, err := LoadDaemonConfig(path)
		if err == nil {
			t.Fatal("LoadDaemonConfig() error = nil, want error for an empty DAEMON_TOKEN")
		}
	})

	t.Run("defaults resolve when only the required key is set", func(t *testing.T) {
		t.Parallel()

		path := writeConfigFixture(t, daemonRequiredEnv())

		cfg, err := LoadDaemonConfig(path)
		if err != nil {
			t.Fatalf("LoadDaemonConfig() error = %v", err)
		}

		if cfg.Token != "s3cr3t-token" {
			t.Fatalf("Token = %q, want %q", cfg.Token, "s3cr3t-token")
		}
		if cfg.Addr != defaultDaemonAddr {
			t.Errorf("Addr = %q, want default %q", cfg.Addr, defaultDaemonAddr)
		}
		if cfg.DBPath != defaultDBPath {
			t.Errorf("DBPath = %q, want default %q", cfg.DBPath, defaultDBPath)
		}
		if cfg.StaleAfter != 90*time.Minute {
			t.Errorf("StaleAfter = %v, want 90m", cfg.StaleAfter)
		}
		if cfg.ReadTimeout != 10*time.Second {
			t.Errorf("ReadTimeout = %v, want 10s", cfg.ReadTimeout)
		}
		if cfg.WriteTimeout != 15*time.Second {
			t.Errorf("WriteTimeout = %v, want 15s", cfg.WriteTimeout)
		}
		if cfg.IdleTimeout != 60*time.Second {
			t.Errorf("IdleTimeout = %v, want 60s", cfg.IdleTimeout)
		}
		if cfg.HealthTimeout != 5*time.Second {
			t.Errorf("HealthTimeout = %v, want 5s", cfg.HealthTimeout)
		}
		if cfg.MaxConns != 2 {
			t.Errorf("MaxConns = %d, want 2", cfg.MaxConns)
		}
	})

	t.Run("DAEMON_ADDR override is carried through", func(t *testing.T) {
		t.Parallel()

		env := daemonRequiredEnv()
		env["DAEMON_ADDR"] = "192.168.1.10:8420"
		path := writeConfigFixture(t, env)

		cfg, err := LoadDaemonConfig(path)
		if err != nil {
			t.Fatalf("LoadDaemonConfig() error = %v", err)
		}
		if cfg.Addr != "192.168.1.10:8420" {
			t.Fatalf("Addr = %q, want %q", cfg.Addr, "192.168.1.10:8420")
		}
	})

	t.Run("DAEMON_STALE_AFTER override is carried through", func(t *testing.T) {
		t.Parallel()

		env := daemonRequiredEnv()
		env["DAEMON_STALE_AFTER"] = "45m"
		path := writeConfigFixture(t, env)

		cfg, err := LoadDaemonConfig(path)
		if err != nil {
			t.Fatalf("LoadDaemonConfig() error = %v", err)
		}
		if cfg.StaleAfter != 45*time.Minute {
			t.Fatalf("StaleAfter = %v, want 45m", cfg.StaleAfter)
		}
	})

	t.Run("invalid DAEMON_STALE_AFTER returns an error naming it", func(t *testing.T) {
		t.Parallel()

		env := daemonRequiredEnv()
		env["DAEMON_STALE_AFTER"] = "not-a-duration"
		path := writeConfigFixture(t, env)

		_, err := LoadDaemonConfig(path)
		if err == nil {
			t.Fatal("LoadDaemonConfig() error = nil, want error for invalid DAEMON_STALE_AFTER")
		}
		if !strings.Contains(err.Error(), "DAEMON_STALE_AFTER") {
			t.Fatalf("LoadDaemonConfig() error = %q, want it to mention DAEMON_STALE_AFTER", err)
		}
	})

	t.Run("DAEMON_MAX_CONNS override is carried through", func(t *testing.T) {
		t.Parallel()

		env := daemonRequiredEnv()
		env["DAEMON_MAX_CONNS"] = "4"
		path := writeConfigFixture(t, env)

		cfg, err := LoadDaemonConfig(path)
		if err != nil {
			t.Fatalf("LoadDaemonConfig() error = %v", err)
		}
		if cfg.MaxConns != 4 {
			t.Fatalf("MaxConns = %d, want 4", cfg.MaxConns)
		}
	})

	t.Run("invalid DAEMON_MAX_CONNS returns an error naming it", func(t *testing.T) {
		t.Parallel()

		env := daemonRequiredEnv()
		env["DAEMON_MAX_CONNS"] = "many"
		path := writeConfigFixture(t, env)

		_, err := LoadDaemonConfig(path)
		if err == nil {
			t.Fatal("LoadDaemonConfig() error = nil, want error for invalid DAEMON_MAX_CONNS")
		}
		if !strings.Contains(err.Error(), "DAEMON_MAX_CONNS") {
			t.Fatalf("LoadDaemonConfig() error = %q, want it to mention DAEMON_MAX_CONNS", err)
		}
	})

	t.Run("never requires router, Telegram, or check config", func(t *testing.T) {
		t.Parallel()

		// only DAEMON_TOKEN is set; LoadConfig's four required keys (LABEL,
		// TELEGRAMBOT_DSN, ROUTER_USER, ROUTER_PASS) are absent and must
		// never be demanded by the daemon's loader.
		path := writeConfigFixture(t, daemonRequiredEnv())

		if _, err := LoadDaemonConfig(path); err != nil {
			t.Fatalf("LoadDaemonConfig() error = %v, want nil (must not require collector-only keys)", err)
		}
	})

	t.Run("router probe disabled when neither cred is set", func(t *testing.T) {
		t.Parallel()

		path := writeConfigFixture(t, daemonRequiredEnv())
		cfg, err := LoadDaemonConfig(path)
		if err != nil {
			t.Fatalf("LoadDaemonConfig() error = %v", err)
		}
		if cfg.RouterHealth != nil {
			t.Fatalf("RouterHealth = %+v, want nil when no creds set", cfg.RouterHealth)
		}
	})

	t.Run("router probe disabled when both creds are empty string", func(t *testing.T) {
		t.Parallel()

		env := daemonRequiredEnv()
		env["ROUTER_USER"] = ""
		env["ROUTER_PASS"] = ""
		path := writeConfigFixture(t, env)

		cfg, err := LoadDaemonConfig(path)
		if err != nil {
			t.Fatalf("LoadDaemonConfig() error = %v", err)
		}
		if cfg.RouterHealth != nil {
			t.Fatalf("RouterHealth = %+v, want nil for empty creds", cfg.RouterHealth)
		}
	})

	t.Run("router probe enabled when both creds are present", func(t *testing.T) {
		t.Parallel()

		env := daemonRequiredEnv()
		env["ROUTER_USER"] = "admin"
		env["ROUTER_PASS"] = "secret"
		path := writeConfigFixture(t, env)

		cfg, err := LoadDaemonConfig(path)
		if err != nil {
			t.Fatalf("LoadDaemonConfig() error = %v", err)
		}
		if cfg.RouterHealth == nil {
			t.Fatal("RouterHealth = nil, want non-nil when both creds are set")
		}
		if cfg.RouterHealth.User != "admin" {
			t.Errorf("RouterHealth.User = %q, want %q", cfg.RouterHealth.User, "admin")
		}
		if cfg.RouterHealth.Pass != "secret" {
			t.Errorf("RouterHealth.Pass = %q, want %q", cfg.RouterHealth.Pass, "secret")
		}
		if cfg.RouterHealth.Kind != domain.RouterKindNokia {
			t.Errorf("RouterHealth.Kind = %q, want default %q", cfg.RouterHealth.Kind, domain.RouterKindNokia)
		}
		if cfg.RouterHealth.Addr != defaultRouterAddr {
			t.Errorf("RouterHealth.Addr = %q, want default %q", cfg.RouterHealth.Addr, defaultRouterAddr)
		}
		if cfg.RouterHealth.ProbeName != defaultRouterProbeName {
			t.Errorf("RouterHealth.ProbeName = %q, want default %q", cfg.RouterHealth.ProbeName, defaultRouterProbeName)
		}
		if cfg.RouterHealth.HTTPTimeout != 3*time.Second {
			t.Errorf("RouterHealth.HTTPTimeout = %v, want 3s (defaultRouterHTTPTimeout)", cfg.RouterHealth.HTTPTimeout)
		}
	})

	t.Run("router probe name override via DAEMON_HEALTH_ROUTER", func(t *testing.T) {
		t.Parallel()

		env := daemonRequiredEnv()
		env["ROUTER_USER"] = "admin"
		env["ROUTER_PASS"] = "secret"
		env["DAEMON_HEALTH_ROUTER"] = "gipon"
		path := writeConfigFixture(t, env)

		cfg, err := LoadDaemonConfig(path)
		if err != nil {
			t.Fatalf("LoadDaemonConfig() error = %v", err)
		}
		if cfg.RouterHealth == nil {
			t.Fatal("RouterHealth = nil")
		}
		if cfg.RouterHealth.ProbeName != "gipon" {
			t.Errorf("RouterHealth.ProbeName = %q, want %q", cfg.RouterHealth.ProbeName, "gipon")
		}
	})

	t.Run("partial creds user-only returns an error naming the missing key", func(t *testing.T) {
		t.Parallel()

		env := daemonRequiredEnv()
		env["ROUTER_USER"] = "admin"
		path := writeConfigFixture(t, env)

		_, err := LoadDaemonConfig(path)
		if err == nil {
			t.Fatal("LoadDaemonConfig() error = nil, want error for user without pass")
		}
		if !strings.Contains(err.Error(), "ROUTER_PASS") {
			t.Fatalf("LoadDaemonConfig() error = %q, want it to mention ROUTER_PASS", err)
		}
	})

	t.Run("partial creds pass-only returns an error naming the missing key", func(t *testing.T) {
		t.Parallel()

		env := daemonRequiredEnv()
		env["ROUTER_PASS"] = "secret"
		path := writeConfigFixture(t, env)

		_, err := LoadDaemonConfig(path)
		if err == nil {
			t.Fatal("LoadDaemonConfig() error = nil, want error for pass without user")
		}
		if !strings.Contains(err.Error(), "ROUTER_USER") {
			t.Fatalf("LoadDaemonConfig() error = %q, want it to mention ROUTER_USER", err)
		}
	})

	t.Run("invalid ROUTER_KIND with creds returns an error", func(t *testing.T) {
		t.Parallel()

		env := daemonRequiredEnv()
		env["ROUTER_USER"] = "admin"
		env["ROUTER_PASS"] = "secret"
		env["ROUTER_KIND"] = "tapo"
		path := writeConfigFixture(t, env)

		_, err := LoadDaemonConfig(path)
		if err == nil {
			t.Fatal("LoadDaemonConfig() error = nil, want error for unsupported ROUTER_KIND")
		}
		if !strings.Contains(err.Error(), "ROUTER_KIND") {
			t.Fatalf("LoadDaemonConfig() error = %q, want it to mention ROUTER_KIND", err)
		}
	})

	t.Run("invalid CHECK_TIMEOUT with creds returns an error", func(t *testing.T) {
		t.Parallel()

		env := daemonRequiredEnv()
		env["ROUTER_USER"] = "admin"
		env["ROUTER_PASS"] = "secret"
		env["CHECK_TIMEOUT"] = "not-a-duration"
		path := writeConfigFixture(t, env)

		_, err := LoadDaemonConfig(path)
		if err == nil {
			t.Fatal("LoadDaemonConfig() error = nil, want error for invalid CHECK_TIMEOUT")
		}
		if !strings.Contains(err.Error(), "CHECK_TIMEOUT") {
			t.Fatalf("LoadDaemonConfig() error = %q, want it to mention CHECK_TIMEOUT", err)
		}
	})

	t.Run("DAEMON_TOKEN is still the sole unconditionally required key", func(t *testing.T) {
		t.Parallel()

		// confirm the daemon starts with only DAEMON_TOKEN — no router keys,
		// no collector keys.
		path := writeConfigFixture(t, daemonRequiredEnv())
		if _, err := LoadDaemonConfig(path); err != nil {
			t.Fatalf("LoadDaemonConfig() error = %v, want nil (only DAEMON_TOKEN should be required)", err)
		}
	})

	t.Run("default router HTTP timeout is 3s", func(t *testing.T) {
		t.Parallel()

		env := daemonRequiredEnv()
		env["ROUTER_USER"] = "admin"
		env["ROUTER_PASS"] = "secret"
		path := writeConfigFixture(t, env)

		cfg, err := LoadDaemonConfig(path)
		if err != nil {
			t.Fatalf("LoadDaemonConfig() error = %v", err)
		}
		if cfg.RouterHealth == nil {
			t.Fatal("RouterHealth = nil, want non-nil")
		}
		if cfg.RouterHealth.HTTPTimeout != 3*time.Second {
			t.Fatalf("RouterHealth.HTTPTimeout = %v, want 3s (default router HTTP timeout)", cfg.RouterHealth.HTTPTimeout)
		}
	})

	t.Run("CHECK_TIMEOUT zero is rejected for the router probe", func(t *testing.T) {
		t.Parallel()

		env := daemonRequiredEnv()
		env["ROUTER_USER"] = "admin"
		env["ROUTER_PASS"] = "secret"
		env["CHECK_TIMEOUT"] = "0s"
		path := writeConfigFixture(t, env)

		_, err := LoadDaemonConfig(path)
		if err == nil {
			t.Fatal("LoadDaemonConfig() error = nil, want error for zero CHECK_TIMEOUT")
		}
		if !strings.Contains(err.Error(), "CHECK_TIMEOUT") {
			t.Fatalf("LoadDaemonConfig() error = %q, want it to mention CHECK_TIMEOUT", err)
		}
	})

	t.Run("CHECK_TIMEOUT equal to DAEMON_HEALTH_TIMEOUT is rejected", func(t *testing.T) {
		t.Parallel()

		env := daemonRequiredEnv()
		env["ROUTER_USER"] = "admin"
		env["ROUTER_PASS"] = "secret"
		env["CHECK_TIMEOUT"] = "5s" // equals DAEMON_HEALTH_TIMEOUT default (5s)
		env["DAEMON_HEALTH_TIMEOUT"] = "5s"
		path := writeConfigFixture(t, env)

		_, err := LoadDaemonConfig(path)
		if err == nil {
			t.Fatal("LoadDaemonConfig() error = nil, want error when CHECK_TIMEOUT >= DAEMON_HEALTH_TIMEOUT")
		}
		if !strings.Contains(err.Error(), "CHECK_TIMEOUT") {
			t.Fatalf("LoadDaemonConfig() error = %q, want it to mention CHECK_TIMEOUT", err)
		}
	})

	t.Run("CHECK_TIMEOUT less than DAEMON_HEALTH_TIMEOUT passes", func(t *testing.T) {
		t.Parallel()

		env := daemonRequiredEnv()
		env["ROUTER_USER"] = "admin"
		env["ROUTER_PASS"] = "secret"
		env["CHECK_TIMEOUT"] = "2s"
		env["DAEMON_HEALTH_TIMEOUT"] = "5s"
		path := writeConfigFixture(t, env)

		cfg, err := LoadDaemonConfig(path)
		if err != nil {
			t.Fatalf("LoadDaemonConfig() error = %v, want nil for valid CHECK_TIMEOUT < DAEMON_HEALTH_TIMEOUT", err)
		}
		if cfg.RouterHealth.HTTPTimeout != 2*time.Second {
			t.Fatalf("RouterHealth.HTTPTimeout = %v, want 2s", cfg.RouterHealth.HTTPTimeout)
		}
	})
}

// daemonRequiredEnv returns a fresh map holding exactly the daemon's one
// required key, so each subtest can mutate its own copy without affecting
// others (mirrors config_test.go's requiredOnlyEnv for LoadConfig).
func daemonRequiredEnv() map[string]string {
	return map[string]string{"DAEMON_TOKEN": "s3cr3t-token"}
}
