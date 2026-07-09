package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/prorochestvo/yarddog/gateway/dto"
	"github.com/prorochestvo/yarddog/services"
)

// ErrLoginFailed reports that login.cgi did not grant a session: bad
// credentials, or a router response this client doesn't recognize. Reboot
// wraps it with %w so callers can errors.Is against it, though every reboot
// failure maps to the same outcome='reboot_failed' regardless of which of
// these two errors caused it (design §14).
var ErrLoginFailed = errors.New("router: login failed (bad credentials or no session cookie set)")

// ErrRebootNotConfirmed reports that reboot.cgi did not confirm the reboot: a
// confirmed reboot answers 200 with the "done reboot" marker in the body. It is
// returned either when the firmware redirected the POST back to the login page
// (3xx Location:/ — a stale or missing CSRF token/session) or when a 200 body
// lacked the marker (README "Router compatibility").
var ErrRebootNotConfirmed = errors.New("router: reboot not confirmed")

// NewNokiaDriver builds a NokiaDriver for the Nokia G-240W-A web UI at addr
// (e.g. "http://192.168.1.1"). The returned client carries its own cookiejar
// so whatever session cookie login.cgi sets — sid or lsid, README "Router
// compatibility" — is forwarded automatically to the reboot.cgi request
// without this client hardcoding either name. timeout bounds every request;
// a router mid-reboot otherwise hangs the connection indefinitely.
func NewNokiaDriver(addr, user, pass string, timeout time.Duration) (*NokiaDriver, error) {
	base, err := url.Parse(addr)
	if err != nil {
		return nil, fmt.Errorf("router: parse address %q: %w", addr, err)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("router: address %q has no host", addr)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("router: create cookie jar: %w", err)
	}

	return &NokiaDriver{
		baseURL: base,
		user:    user,
		pass:    pass,
		httpClient: &http.Client{
			Timeout: timeout,
			Jar:     jar,
			// do not auto-follow redirects: login.cgi and a rejected reboot.cgi
			// both answer 302. login only needs the Set-Cookie (the jar captures
			// it before any redirect is considered), and Reboot must SEE the 302
			// bounce to distinguish a rejected reboot from an accepted one —
			// following it would mask the failure as login-page HTML (the bug
			// this driver had before the CSRF fix).
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// NokiaDriver drives the Nokia G-240W-A reboot flow over its plain-HTTP web
// UI (README "Router compatibility"): POST login.cgi to obtain a session
// cookie, GET reboot.cgi to read the fresh per-request CSRF token, then POST
// reboot.cgi?reboot echoing that token in the form body. Reboot implements
// services.Rebooter.
type NokiaDriver struct {
	baseURL    *url.URL
	user       string
	pass       string
	httpClient *http.Client
}

var _ services.Rebooter = (*NokiaDriver)(nil)
var _ services.Credentialer = (*NokiaDriver)(nil)

// CheckCredentials proves the configured credentials still authenticate against
// the router web UI: it performs the same login.cgi POST as Reboot (success =
// a session cookie was set in the jar) and then resets the cookie jar so no
// live session lingers. The session-jar reset is the "logout" — there is no
// documented /logout.cgi on the Nokia G-240W-A (gateway/dto/nokia.go: do not
// invent bytes), so clearing the jar locally is the safe MVP. A jar-reset
// failure is impossible (jar.New is the constructor; Clear just swaps the
// pointer), so the "logout failure is swallowed" contract is naturally
// satisfied without needing to discard an error. Implements services.Credentialer.
func (d *NokiaDriver) CheckCredentials(ctx context.Context) error {
	if err := d.login(ctx); err != nil {
		return fmt.Errorf("router: check credentials: %w", err)
	}
	d.logout()
	return nil
}

// Reboot logs into the router and requests a reboot (design §5). It does not
// log out afterward — the session dies with the reboot, and a logout attempt
// against a router that is already rebooting would only produce a spurious
// error (design §5).
func (d *NokiaDriver) Reboot(ctx context.Context) error {
	if err := d.login(ctx); err != nil {
		return fmt.Errorf("router: reboot: %w", err)
	}

	token, err := d.fetchCSRFToken(ctx)
	if err != nil {
		return fmt.Errorf("router: reboot: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.buildURL("/reboot.cgi", "reboot"), strings.NewReader(dto.EncodeRebootBody(token)))
	if err != nil {
		return fmt.Errorf("router: build reboot request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("router: reboot request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("router: read reboot response: %w", err)
	}

	// a stale/missing CSRF token or session makes the firmware redirect the
	// POST back to the login page (302 Location:/) instead of rebooting.
	// redirects are disabled on this client (NewNokiaDriver), so the raw 3xx is
	// visible here instead of being silently followed to login HTML.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return fmt.Errorf("%w: reboot.cgi redirected to %q", ErrRebootNotConfirmed, resp.Header.Get("Location"))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("router: reboot.cgi returned status %d", resp.StatusCode)
	}
	// a confirmed reboot answers 200 with "done reboot" in the body (verified
	// against firmware HJHL16); a 200 without it is an unexpected page, not a
	// reboot.
	if !bytes.Contains(body, []byte(dto.RebootDoneMarker)) {
		return fmt.Errorf("%w: 200 but body lacked %q: %q", ErrRebootNotConfirmed, dto.RebootDoneMarker, body)
	}

	return nil
}

// fetchCSRFToken GETs the reboot.cgi page and extracts the fresh per-request
// CSRF token the firmware requires on the reboot POST. The session cookie set
// by login is attached automatically by the cookie jar. A non-200 status
// means the session was rejected (the router bounced the GET to login), which
// surfaces as an error so the caller never POSTs a reboot without a token.
func (d *NokiaDriver) fetchCSRFToken(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.buildURL("/reboot.cgi", ""), nil)
	if err != nil {
		return "", fmt.Errorf("build reboot-page request: %w", err)
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch reboot page: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read reboot page: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("reboot page returned status %d (session rejected?)", resp.StatusCode)
	}

	token, err := dto.ParseCSRFToken(body)
	if err != nil {
		return "", err
	}
	return token, nil
}

// logout resets the cookie jar to evict the session cookie set by a prior
// login. This is best-effort hygiene — the session will expire on its own, and
// there is no documented /logout.cgi (gateway/dto/nokia.go), so clearing the
// jar is both safe and sufficient. It is the caller's responsibility (CheckCredentials)
// to document that a logout failure never fails the probe; here the reset is
// infallible because we replace the jar pointer atomically.
func (d *NokiaDriver) logout() {
	jar, err := cookiejar.New(nil)
	if err != nil {
		// cookiejar.New only fails when the Options pointer is invalid, which
		// cannot happen here (nil is the documented safe default). If somehow it
		// does fail, the old jar persists — the session expires naturally.
		return
	}
	d.httpClient.Jar = jar
}

// buildURL joins path (e.g. "/login.cgi") and rawQuery (e.g. "reboot", with
// no "=" — the router's query has no value, only a bare flag) onto d's base
// address, trimming any trailing slash on the base path first so the result
// never contains a double slash regardless of whether ROUTER_ADDR was
// configured with one.
func (d *NokiaDriver) buildURL(path, rawQuery string) string {
	u := *d.baseURL
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = rawQuery
	return u.String()
}

// login POSTs the configured credentials to login.cgi and checks the
// cookiejar for the session cookie the router is documented to set on
// success (README "Router compatibility"). There is no other documented
// success/failure signal from this endpoint (design risk §14 — the contract
// is inferred), so cookie presence is the sole criterion.
func (d *NokiaDriver) login(ctx context.Context) error {
	form := dto.NokiaLoginForm{Name: d.user, Pswd: d.pass}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.buildURL("/login.cgi", ""), strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("router: build login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("router: login request: %w", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return fmt.Errorf("router: drain login response: %w", err)
	}

	for _, c := range d.httpClient.Jar.Cookies(d.baseURL) {
		if c.Name == dto.CookieNameSID || c.Name == dto.CookieNameLSID {
			return nil
		}
	}

	return ErrLoginFailed
}
