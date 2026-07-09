package telegram

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prorochestvo/yarddog/gateway/dto"
)

func TestParseDSN(t *testing.T) {
	t.Parallel()

	t.Run("valid dsn", func(t *testing.T) {
		t.Parallel()

		chatID, token, err := ParseDSN(testDSN)
		if err != nil {
			t.Fatalf("ParseDSN: %v", err)
		}
		if chatID != testChatID {
			t.Fatalf("chatID = %q, want %q", chatID, testChatID)
		}
		if token != testToken {
			t.Fatalf("token = %q, want %q", token, testToken)
		}
	})

	t.Run("token containing a colon survives", func(t *testing.T) {
		t.Parallel()

		_, token, err := ParseDSN("tbot://1:@123456789:AAExxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx/")
		if err != nil {
			t.Fatalf("ParseDSN: %v", err)
		}
		want := "123456789:AAExxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
		if token != want {
			t.Fatalf("token = %q, want %q (net/url would have mis-split this)", token, want)
		}
	})

	t.Run("missing tbot prefix", func(t *testing.T) {
		t.Parallel()

		dsn := "http://115818690:@NNN:AAA/"
		_, _, err := ParseDSN(dsn)
		if err == nil {
			t.Fatal("ParseDSN: want error for missing tbot:// prefix, got nil")
		}
		if err.Error() != "telegram: dsn missing tbot:// prefix" {
			t.Fatalf("ParseDSN error = %q, want a structural message with no dsn value", err)
		}
		assertNoRawDSNLeak(t, err, dsn)
	})

	t.Run("missing colon-at separator", func(t *testing.T) {
		t.Parallel()

		dsn := "tbot://115818690-NNN-AAA/"
		_, _, err := ParseDSN(dsn)
		if err == nil {
			t.Fatal("ParseDSN: want error for missing :@ separator, got nil")
		}
		if err.Error() != "telegram: dsn missing :@ separator" {
			t.Fatalf("ParseDSN error = %q, want a structural message with no dsn value", err)
		}
		assertNoRawDSNLeak(t, err, dsn)
	})

	t.Run("empty token", func(t *testing.T) {
		t.Parallel()

		dsn := "tbot://115818690:@/"
		_, _, err := ParseDSN(dsn)
		if err == nil {
			t.Fatal("ParseDSN: want error for empty token, got nil")
		}
		if err.Error() != "telegram: dsn has empty bot token" {
			t.Fatalf("ParseDSN error = %q, want a structural message with no dsn value", err)
		}
		assertNoRawDSNLeak(t, err, dsn)
	})

	t.Run("empty chat id", func(t *testing.T) {
		t.Parallel()

		dsn := "tbot://:@NNN:AAA/"
		_, _, err := ParseDSN(dsn)
		if err == nil {
			t.Fatal("ParseDSN: want error for empty chat id, got nil")
		}
		if err.Error() != "telegram: dsn has empty chat id" {
			t.Fatalf("ParseDSN error = %q, want a structural message with no dsn value", err)
		}
		assertNoRawDSNLeak(t, err, dsn)
	})
}

func TestNewClient(t *testing.T) {
	t.Parallel()

	t.Run("valid dsn wires chat id and token", func(t *testing.T) {
		t.Parallel()

		c, err := NewClient(testDSN)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if c.chatID != testChatID {
			t.Fatalf("chatID = %q, want %q", c.chatID, testChatID)
		}
		if c.token != testToken {
			t.Fatalf("token = %q, want %q", c.token, testToken)
		}
	})

	t.Run("invalid dsn is rejected", func(t *testing.T) {
		t.Parallel()

		if _, err := NewClient("not-a-dsn"); err == nil {
			t.Fatal("NewClient: want error for invalid dsn, got nil")
		}
	})
}

func TestClient_Send(t *testing.T) {
	t.Parallel()

	t.Run("posts chat_id and text to the sendMessage endpoint", func(t *testing.T) {
		t.Parallel()

		srv, gotReq, gotPath := newTelegramServer(t, false)
		c := newTestClient(t, srv.URL)

		text := "starting router reboot (reason: no internet)"
		if err := c.Send(t.Context(), text); err != nil {
			t.Fatalf("Send: %v", err)
		}

		wantPath := "/bot" + testToken + "/sendMessage"
		if *gotPath != wantPath {
			t.Fatalf("request path = %q, want %q", *gotPath, wantPath)
		}
		if gotReq.ChatID != testChatID {
			t.Fatalf("chat_id = %q, want %q", gotReq.ChatID, testChatID)
		}
		if gotReq.Text != text {
			t.Fatalf("text = %q, want %q", gotReq.Text, text)
		}
	})

	t.Run("a non-200 response is a non-nil error", func(t *testing.T) {
		t.Parallel()

		srv, _, _ := newTelegramServer(t, true)
		c := newTestClient(t, srv.URL)

		if err := c.Send(t.Context(), "router went down"); err == nil {
			t.Fatal("Send: want a non-nil error for a 500 response, got nil")
		}
	})

	t.Run("a transport failure is redacted so the raw token never appears in the returned error", func(t *testing.T) {
		t.Parallel()

		// close the server before use so every request fails with "connection
		// refused" and the resulting *url.Error embeds the full request URL
		// (including "/bot"+token+"/sendMessage") verbatim, unredacted by
		// net/http (P0-1).
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		unreachableURL := srv.URL
		srv.Close()

		c := newTestClient(t, unreachableURL)

		err := c.Send(t.Context(), "router went down")
		if err == nil {
			t.Fatal("Send: want a non-nil error for an unreachable host, got nil")
		}
		if strings.Contains(err.Error(), testToken) {
			t.Fatalf("Send error = %q, want the bot token %q redacted", err, testToken)
		}
		if !strings.Contains(err.Error(), "***") {
			t.Fatalf("Send error = %q, want the redacted placeholder \"***\" in place of the token", err)
		}
	})
}

const (
	testDSN    = "tbot://115818690:@NNN:AAA/"
	testChatID = "115818690"
	testToken  = "NNN:AAA"
)

// assertNoRawDSNLeak fails the test if err's message contains dsn verbatim
// (P0-2: ParseDSN must report which structural part failed, never the DSN's
// chat id/token value, since main logs its error as-is).
func assertNoRawDSNLeak(t *testing.T, err error, dsn string) {
	t.Helper()

	if strings.Contains(err.Error(), dsn) {
		t.Fatalf("error %q leaks the raw dsn %q", err.Error(), dsn)
	}
}

// newTelegramServer starts an httptest.Server that decodes each sendMessage
// POST body into gotReq and records its path into gotPath, responding 200
// unless fail is true (500), so tests can drive both the success and
// failure paths without touching the real Telegram API.
func newTelegramServer(t *testing.T, fail bool) (srv *httptest.Server, gotReq *dto.TelegramSendMessageRequest, gotPath *string) {
	t.Helper()

	gotReq = &dto.TelegramSendMessageRequest{}
	gotPath = new(string)

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(gotReq); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	return srv, gotReq, gotPath
}

// newTestClient builds a Client over testDSN pointed at baseURL instead of
// the real Telegram API.
func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()

	c, err := NewClient(testDSN)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c.baseURL = baseURL

	return c
}
