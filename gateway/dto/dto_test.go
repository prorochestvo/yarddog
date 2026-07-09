package dto

import (
	"encoding/json"
	"errors"
	"net/url"
	"testing"
)

func TestNokiaLoginForm_Encode(t *testing.T) {
	t.Parallel()

	t.Run("matches a manually built url.Values encoding", func(t *testing.T) {
		t.Parallel()

		f := NokiaLoginForm{Name: "admin", Pswd: "secret"}

		got := f.Encode()

		want := url.Values{"name": {"admin"}, "pswd": {"secret"}}.Encode()
		if got != want {
			t.Fatalf("Encode() = %q, want %q", got, want)
		}
	})

	t.Run("round-trips through url.ParseQuery", func(t *testing.T) {
		t.Parallel()

		f := NokiaLoginForm{Name: "admin", Pswd: "s3cr3t!"}

		got, err := url.ParseQuery(f.Encode())
		if err != nil {
			t.Fatalf("ParseQuery(%q): %v", f.Encode(), err)
		}
		if got.Get("name") != f.Name {
			t.Fatalf("name = %q, want %q", got.Get("name"), f.Name)
		}
		if got.Get("pswd") != f.Pswd {
			t.Fatalf("pswd = %q, want %q", got.Get("pswd"), f.Pswd)
		}
	})
}

func TestParseCSRFToken(t *testing.T) {
	t.Parallel()

	t.Run("extracts the token from the hidden input", func(t *testing.T) {
		t.Parallel()

		page := []byte(`<form></form><script>$("form").prepend('<input type="hidden" name="csrf_token" value="aSrAmhUSmUfCdWgZ" />');</script>`)

		got, err := ParseCSRFToken(page)
		if err != nil {
			t.Fatalf("ParseCSRFToken: %v", err)
		}
		if got != "aSrAmhUSmUfCdWgZ" {
			t.Fatalf("token = %q, want %q", got, "aSrAmhUSmUfCdWgZ")
		}
	})

	t.Run("does not match the bare csrf_token JS literal", func(t *testing.T) {
		t.Parallel()

		// the reboot page carries csrf_token=… inside inline JS as well as the
		// hidden input; on a page that has only the JS literal the parser must
		// not mistake it for a token.
		page := []byte(`<script>settings.data = 'csrf_token=SHOULDNOTMATCH';</script>`)

		if _, err := ParseCSRFToken(page); !errors.Is(err, ErrCSRFTokenNotFound) {
			t.Fatalf("ParseCSRFToken error = %v, want ErrCSRFTokenNotFound", err)
		}
	})

	t.Run("returns ErrCSRFTokenNotFound when absent", func(t *testing.T) {
		t.Parallel()

		if _, err := ParseCSRFToken([]byte("<html>no token</html>")); !errors.Is(err, ErrCSRFTokenNotFound) {
			t.Fatalf("ParseCSRFToken error = %v, want ErrCSRFTokenNotFound", err)
		}
	})
}

func TestEncodeRebootBody(t *testing.T) {
	t.Parallel()

	t.Run("carries the token under csrf_token", func(t *testing.T) {
		t.Parallel()

		vals, err := url.ParseQuery(EncodeRebootBody("aSrAmhUSmUfCdWgZ"))
		if err != nil {
			t.Fatalf("ParseQuery: %v", err)
		}
		if got := vals.Get(CSRFTokenField); got != "aSrAmhUSmUfCdWgZ" {
			t.Fatalf("csrf_token = %q, want %q", got, "aSrAmhUSmUfCdWgZ")
		}
	})

	t.Run("preserves the button's data prefix for byte-fidelity", func(t *testing.T) {
		t.Parallel()

		// the router's Reboot button POSTs data:'data' and the ajaxSend
		// prefilter appends &csrf_token=…; the exact captured shape must survive.
		if got := EncodeRebootBody("X"); got != "data&csrf_token=X" {
			t.Fatalf("EncodeRebootBody = %q, want %q", got, "data&csrf_token=X")
		}
	})
}

func TestTelegramSendMessageRequest(t *testing.T) {
	t.Parallel()

	t.Run("marshals chat_id and text", func(t *testing.T) {
		t.Parallel()

		req := TelegramSendMessageRequest{ChatID: "115818690", Text: "hello"}

		body, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		want := `{"chat_id":"115818690","text":"hello"}`
		if string(body) != want {
			t.Fatalf("Marshal() = %s, want %s", body, want)
		}
	})

	t.Run("round-trips through Unmarshal", func(t *testing.T) {
		t.Parallel()

		body := []byte(`{"chat_id":"42","text":"router went down"}`)

		var got TelegramSendMessageRequest
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}

		want := TelegramSendMessageRequest{ChatID: "42", Text: "router went down"}
		if got != want {
			t.Fatalf("Unmarshal() = %+v, want %+v", got, want)
		}
	})
}

func TestRunDTO_JSON(t *testing.T) {
	t.Parallel()

	t.Run("RunDTO with every optional field unset marshals them all to null", func(t *testing.T) {
		t.Parallel()

		run := RunDTO{
			ID:        128,
			StartedAt: "2026-07-07T04:07:00Z",
			Mode:      "hard",
			Action:    "none",
		}

		body, err := json.Marshal(run)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		want := `{"id":128,"started_at":"2026-07-07T04:07:00Z","mode":"hard","internet_ok":null,` +
			`"action":"none","reboot_started_at":null,"router_down_at":null,"router_up_at":null,` +
			`"internet_restored_at":null,"finished_at":null,"outcome":"","error":""}`
		if string(body) != want {
			t.Fatalf("Marshal() = %s, want %s", body, want)
		}
	})

	t.Run("RunDTO with every optional field set marshals RFC3339 strings", func(t *testing.T) {
		t.Parallel()

		internetOK := true
		rebootStartedAt := "2026-07-07T04:07:01Z"
		finishedAt := "2026-07-07T04:07:03Z"

		run := RunDTO{
			ID:              128,
			StartedAt:       "2026-07-07T04:07:00Z",
			Mode:            "soft",
			InternetOK:      &internetOK,
			Action:          "reboot",
			RebootStartedAt: &rebootStartedAt,
			FinishedAt:      &finishedAt,
			Outcome:         "ok",
		}

		body, err := json.Marshal(run)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		var got RunDTO
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if got.RebootStartedAt == nil || *got.RebootStartedAt != rebootStartedAt {
			t.Fatalf("RebootStartedAt = %v, want %q", got.RebootStartedAt, rebootStartedAt)
		}
		if got.FinishedAt == nil || *got.FinishedAt != finishedAt {
			t.Fatalf("FinishedAt = %v, want %q", got.FinishedAt, finishedAt)
		}
		if got.InternetOK == nil || !*got.InternetOK {
			t.Fatalf("InternetOK = %v, want true", got.InternetOK)
		}
		// pointer fields already checked by value above; compare the rest of
		// the struct as scalars (a bare != would compare pointer identity,
		// not the pointed-to values, since RunDTO holds several *string/*bool
		// fields).
		got.InternetOK, got.RebootStartedAt, got.FinishedAt = nil, nil, nil
		run.InternetOK, run.RebootStartedAt, run.FinishedAt = nil, nil, nil
		if got != run {
			t.Fatalf("round-tripped RunDTO (pointer fields zeroed for comparison) = %+v, want %+v", got, run)
		}
	})
}

func TestMetricDTO_JSON(t *testing.T) {
	t.Parallel()

	t.Run("MetricDTO with an unavailable sample marshals value null", func(t *testing.T) {
		t.Parallel()

		m := MetricDTO{
			RunID:     128,
			TS:        "2026-07-07T04:07:00Z",
			Collector: "fans",
			Name:      "fans",
			Unit:      "rpm",
			OK:        false,
			Error:     "no fan sensors present",
		}

		body, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		want := `{"run_id":128,"ts":"2026-07-07T04:07:00Z","collector":"fans","name":"fans",` +
			`"value":null,"unit":"rpm","ok":false,"error":"no fan sensors present"}`
		if string(body) != want {
			t.Fatalf("Marshal() = %s, want %s", body, want)
		}
	})

	t.Run("MetricDTO with an ok sample marshals its value", func(t *testing.T) {
		t.Parallel()

		value := 52.35
		m := MetricDTO{
			RunID:     128,
			TS:        "2026-07-07T04:07:00Z",
			Collector: "temperature",
			Name:      "cpu-thermal",
			Value:     &value,
			Unit:      "celsius",
			OK:        true,
		}

		body, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		want := `{"run_id":128,"ts":"2026-07-07T04:07:00Z","collector":"temperature","name":"cpu-thermal",` +
			`"value":52.35,"unit":"celsius","ok":true,"error":""}`
		if string(body) != want {
			t.Fatalf("Marshal() = %s, want %s", body, want)
		}
	})
}

func TestHealthResponse_JSON(t *testing.T) {
	t.Parallel()

	t.Run("HealthResponse marshals server and per-dependency services", func(t *testing.T) {
		t.Parallel()

		h := HealthResponse{
			Status:   false,
			Server:   HealthServer{Version: "v0.2.0", Uptime: "2h34m12s"},
			Services: map[string]string{"sqlite": "ok"},
		}

		body, err := json.Marshal(h)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		want := `{"status":false,"server":{"version":"v0.2.0","uptime":"2h34m12s"},"services":{"sqlite":"ok"}}`
		if string(body) != want {
			t.Fatalf("Marshal() = %s, want %s", body, want)
		}
	})
}

func TestErrorResponse_JSON(t *testing.T) {
	t.Parallel()

	t.Run("ErrorResponse marshals a single error field", func(t *testing.T) {
		t.Parallel()

		body, err := json.Marshal(ErrorResponse{Error: "run not found"})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		want := `{"error":"run not found"}`
		if string(body) != want {
			t.Fatalf("Marshal() = %s, want %s", body, want)
		}
	})
}
