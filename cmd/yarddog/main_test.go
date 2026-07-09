package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireLock(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "yarddog.lock")

	f, err := acquireLock(path)
	if err != nil {
		t.Fatalf("acquireLock(%q) = %v, want nil error", path, err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("close lock file: %v", err)
		}
	})

	if _, err := acquireLock(path); err == nil {
		t.Fatal("acquireLock on an already-held lock = nil error, want contention error")
	}
}

func TestLockPathFor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		dbPath string
		want   string
	}{
		{"keeps the lock beside a production db", "/opt/yarddog/yarddog.sqlite", "/opt/yarddog/yarddog.lock"},
		{"default db directory", "/var/lib/yarddog/yarddog.db", "/var/lib/yarddog/yarddog.lock"},
		{"relative local db resolves into the working dir", "./yarddog.db", "yarddog.lock"},
		{"bare filename with no directory", "yarddog.db", "yarddog.lock"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := lockPathFor(tc.dbPath); got != tc.want {
				t.Fatalf("lockPathFor(%q) = %q, want %q", tc.dbPath, got, tc.want)
			}
		})
	}
}

func TestResolveEnvPath(t *testing.T) {
	t.Run("explicit flag wins over everything", func(t *testing.T) {
		t.Parallel()

		got := resolveEnvPath("/some/explicit/path")
		if want := "/some/explicit/path"; got != want {
			t.Fatalf("resolveEnvPath(explicit) = %q, want %q", got, want)
		}
	})

	t.Run("falls back when default path is absent", func(t *testing.T) {
		t.Parallel()

		// defaultEnvPath (/opt/yarddog/.env) is not expected to exist in the
		// test sandbox; if it does, this subtest can't observe the fallback
		// branch and is skipped rather than asserting a false negative.
		if _, err := os.Stat(defaultEnvPath); err == nil {
			t.Skipf("%s exists in this environment, cannot exercise the fallback branch", defaultEnvPath)
		}

		got := resolveEnvPath("")
		if got != fallbackEnvPath {
			t.Fatalf("resolveEnvPath(\"\") = %q, want fallback %q", got, fallbackEnvPath)
		}
	})
}
