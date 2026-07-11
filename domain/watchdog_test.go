package domain

import (
	"testing"
)

func TestParseRouterKind(t *testing.T) {
	t.Parallel()

	t.Run("empty string defaults to nokia", func(t *testing.T) {
		t.Parallel()

		got, err := ParseRouterKind("")
		if err != nil {
			t.Fatalf(`ParseRouterKind("") error = %v, want nil`, err)
		}
		if got != RouterKindNokia {
			t.Fatalf(`ParseRouterKind("") = %q, want %q`, got, RouterKindNokia)
		}
	})

	t.Run("nokia is recognized", func(t *testing.T) {
		t.Parallel()

		got, err := ParseRouterKind("nokia")
		if err != nil {
			t.Fatalf(`ParseRouterKind("nokia") error = %v, want nil`, err)
		}
		if got != RouterKindNokia {
			t.Fatalf(`ParseRouterKind("nokia") = %q, want %q`, got, RouterKindNokia)
		}
	})

	t.Run("mixed case and surrounding space are normalized", func(t *testing.T) {
		t.Parallel()

		got, err := ParseRouterKind("  Nokia  ")
		if err != nil {
			t.Fatalf("ParseRouterKind error = %v, want nil", err)
		}
		if got != RouterKindNokia {
			t.Fatalf(`ParseRouterKind("  Nokia  ") = %q, want %q`, got, RouterKindNokia)
		}
	})

	t.Run("unknown kind is an error, never a silent fallback to nokia", func(t *testing.T) {
		t.Parallel()

		_, err := ParseRouterKind("tapo")
		if err == nil {
			t.Fatal(`ParseRouterKind("tapo") error = nil, want an error for an unrecognized kind`)
		}
	})
}
