package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/prorochestvo/yarddog/gateway/dto"
	"github.com/prorochestvo/yarddog/services"
)

// NewClient parses dsn (design §8.1, "tbot://{chat_id}:@{bot_token}/") and
// returns a Client pointed at the real Telegram API; tests in this package
// override the unexported baseURL field to redirect requests at an httptest
// server instead.
func NewClient(dsn string) (*Client, error) {
	chatID, token, err := ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("telegram: %w", err)
	}

	return &Client{
		chatID:     chatID,
		token:      token,
		baseURL:    telegramBaseURL,
		httpClient: &http.Client{Timeout: sendTimeout},
	}, nil
}

// ParseDSN splits the DSN "tbot://{chat_id}:@{bot_token}/" into chat id and
// bot token (design §8.1). net/url is deliberately not used: a bot token
// legitimately contains a ':' (form "NNN:AAA"), which url.Parse would
// mis-split as userinfo/host.
func ParseDSN(dsn string) (chatID, token string, err error) {
	rest, ok := strings.CutPrefix(dsn, "tbot://")
	if !ok {
		return "", "", fmt.Errorf("telegram: dsn missing tbot:// prefix")
	}

	rest = strings.TrimSuffix(rest, "/")

	chatID, token, ok = strings.Cut(rest, ":@")
	if !ok {
		return "", "", fmt.Errorf("telegram: dsn missing :@ separator")
	}
	if chatID == "" {
		return "", "", fmt.Errorf("telegram: dsn has empty chat id")
	}
	if token == "" {
		return "", "", fmt.Errorf("telegram: dsn has empty bot token")
	}

	return chatID, token, nil
}

// Client sends one message at a time through the Telegram Bot API's
// sendMessage endpoint. It implements services.Sender; OutboxService
// (services/outbox.go) is its only caller — the outbox queueing,
// enqueue-before-send ordering, and "[queued HH:MM]" annotation all live
// there, not here.
type Client struct {
	chatID     string
	token      string
	baseURL    string
	httpClient *http.Client
}

var _ services.Sender = (*Client)(nil)

// Send posts text to the Telegram Bot API under c's chat id (design §8.2).
// A transport error's *url.Error embeds the full request URL verbatim
// (net/http never redacts path segments), and that URL contains c.token, so
// any failure is redacted here before it is returned: OutboxService
// persists whatever Send gives back verbatim, so the raw token must never
// survive past this call. Returning a fresh error (not %w-wrapping
// sendMessage's) is deliberate — it guarantees no unredacted value reachable
// via errors.Unwrap/errors.As survives either.
func (c *Client) Send(ctx context.Context, text string) error {
	if err := c.sendMessage(ctx, text); err != nil {
		return errors.New(c.redact(err.Error()))
	}
	return nil
}

const (
	// telegramBaseURL is the real Telegram Bot API host, NewClient's
	// default. Tests override Client.baseURL directly (same package) to
	// redirect requests at an httptest server instead.
	telegramBaseURL = "https://api.telegram.org"

	// sendTimeout bounds every sendMessage call (design §8.2); a hung
	// Telegram API otherwise stalls the run indefinitely.
	sendTimeout = 10 * time.Second
)

// redact replaces every occurrence of c's bot token in s with "***", so a
// send error is never returned with the token still readable (see Send).
func (c *Client) redact(s string) string {
	return strings.ReplaceAll(s, c.token, "***")
}

// sendMessage POSTs text to the Telegram Bot API sendMessage endpoint
// (design §8.2) under c's chat id. Errors returned here may embed the raw
// bot token (e.g. inside a *url.Error's URL) — Send redacts before any of
// this ever reaches a caller.
func (c *Client) sendMessage(ctx context.Context, text string) error {
	body, err := json.Marshal(dto.TelegramSendMessageRequest{ChatID: c.chatID, Text: text})
	if err != nil {
		return fmt.Errorf("telegram: marshal request: %w", err)
	}

	url := c.baseURL + "/bot" + c.token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if readErr != nil {
			return fmt.Errorf("telegram: send message: status %d, read response: %w", resp.StatusCode, readErr)
		}
		return fmt.Errorf("telegram: send message: status %d: %s", resp.StatusCode, respBody)
	}

	return nil
}
