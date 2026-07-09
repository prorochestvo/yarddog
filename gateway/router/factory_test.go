package router

import (
	"testing"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

func TestNew(t *testing.T) {
	t.Parallel()

	cfg := RouterConfig{Addr: "http://192.168.1.1", User: "admin", Pass: "secret", Timeout: time.Second}

	t.Run("empty kind defaults to a working nokia driver", func(t *testing.T) {
		t.Parallel()

		rb, err := New("", cfg)
		if err != nil {
			t.Fatalf(`New("", cfg) error = %v, want nil`, err)
		}
		if _, ok := rb.(*NokiaDriver); !ok {
			t.Fatalf(`New("", cfg) = %T, want *NokiaDriver`, rb)
		}
	})

	t.Run("explicit nokia returns a working nokia driver", func(t *testing.T) {
		t.Parallel()

		rb, err := New(domain.RouterKindNokia, cfg)
		if err != nil {
			t.Fatalf("New(RouterKindNokia, cfg) error = %v, want nil", err)
		}
		if _, ok := rb.(*NokiaDriver); !ok {
			t.Fatalf("New(RouterKindNokia, cfg) = %T, want *NokiaDriver", rb)
		}
	})

	t.Run("unknown kind is an error and a nil rebooter", func(t *testing.T) {
		t.Parallel()

		rb, err := New(domain.RouterKind("tapo"), cfg)
		if err == nil {
			t.Fatal(`New("tapo", cfg) error = nil, want an error for an unsupported kind`)
		}
		if rb != nil {
			t.Fatalf(`New("tapo", cfg) rebooter = %v, want nil`, rb)
		}
	})
}
