package infrastructure

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	t.Parallel()

	t.Run("skips blank lines and comments", func(t *testing.T) {
		t.Parallel()

		path := writeEnvFile(t, "\n# a comment\n\nLABEL=home\n  # indented comment\nROUTER_USER=admin\n")

		got, err := loadEnvFile(path)
		if err != nil {
			t.Fatalf("loadEnvFile() error = %v", err)
		}

		want := map[string]string{"LABEL": "home", "ROUTER_USER": "admin"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("loadEnvFile() = %#v, want %#v", got, want)
		}
	})

	t.Run("splits on first equals only", func(t *testing.T) {
		t.Parallel()

		path := writeEnvFile(t, "TELEGRAMBOT_DSN=tbot://115818690:@NNN:AAA/\n")

		got, err := loadEnvFile(path)
		if err != nil {
			t.Fatalf("loadEnvFile() error = %v", err)
		}

		want := "tbot://115818690:@NNN:AAA/"
		if got["TELEGRAMBOT_DSN"] != want {
			t.Fatalf("TELEGRAMBOT_DSN = %q, want %q", got["TELEGRAMBOT_DSN"], want)
		}
	})

	t.Run("strips double quotes", func(t *testing.T) {
		t.Parallel()

		path := writeEnvFile(t, `LABEL="home lab"`+"\n")

		got, err := loadEnvFile(path)
		if err != nil {
			t.Fatalf("loadEnvFile() error = %v", err)
		}

		if got["LABEL"] != "home lab" {
			t.Fatalf("LABEL = %q, want %q", got["LABEL"], "home lab")
		}
	})

	t.Run("strips single quotes", func(t *testing.T) {
		t.Parallel()

		path := writeEnvFile(t, "LABEL='home lab'\n")

		got, err := loadEnvFile(path)
		if err != nil {
			t.Fatalf("loadEnvFile() error = %v", err)
		}

		if got["LABEL"] != "home lab" {
			t.Fatalf("LABEL = %q, want %q", got["LABEL"], "home lab")
		}
	})

	t.Run("ignores line with no equals sign", func(t *testing.T) {
		t.Parallel()

		path := writeEnvFile(t, "not-a-kv-line\nLABEL=home\n")

		got, err := loadEnvFile(path)
		if err != nil {
			t.Fatalf("loadEnvFile() error = %v", err)
		}

		want := map[string]string{"LABEL": "home"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("loadEnvFile() = %#v, want %#v", got, want)
		}
	})

	t.Run("missing file returns empty map and no error", func(t *testing.T) {
		t.Parallel()

		got, err := loadEnvFile(filepath.Join(t.TempDir(), "does-not-exist.env"))
		if err != nil {
			t.Fatalf("loadEnvFile() error = %v, want nil", err)
		}

		if len(got) != 0 {
			t.Fatalf("loadEnvFile() = %#v, want empty map", got)
		}
	})
}

// writeEnvFile writes contents to a fresh file under t.TempDir() and returns
// its path, so tests never touch a real .env.
func writeEnvFile(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write env fixture: %v", err)
	}

	return path
}
