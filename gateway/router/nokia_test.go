package router

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prorochestvo/yarddog/gateway/dto"
)

func TestNewNokiaDriver(t *testing.T) {
	t.Parallel()

	t.Run("valid address builds a driver", func(t *testing.T) {
		t.Parallel()

		d, err := NewNokiaDriver("http://192.168.1.1", "admin", "secret", time.Second)
		if err != nil {
			t.Fatalf("NewNokiaDriver: %v", err)
		}
		if d.baseURL.Host != "192.168.1.1" {
			t.Fatalf("baseURL.Host = %q, want %q", d.baseURL.Host, "192.168.1.1")
		}
	})

	t.Run("address with no host is an error", func(t *testing.T) {
		t.Parallel()

		if _, err := NewNokiaDriver("not-a-url", "admin", "secret", time.Second); err == nil {
			t.Fatal("NewNokiaDriver: want error for hostless address, got nil")
		}
	})
}

func TestNokiaDriver_Reboot(t *testing.T) {
	t.Parallel()

	t.Run("login, fetch csrf token, reboot POST carries token+cookie+content-type", func(t *testing.T) {
		t.Parallel()

		srv, obs := newRouterServer(t, routerServerConfig{
			validUser:     "admin",
			validPass:     "secret",
			sessionCookie: dto.CookieNameSID,
			csrfToken:     "TOKENabc123",
		})

		d := newTestDriver(t, srv.URL, "admin", "secret")
		if err := d.Reboot(t.Context()); err != nil {
			t.Fatalf("Reboot: %v", err)
		}
		if !obs.getRebootCalled {
			t.Fatal("GET /reboot.cgi was never called to fetch the csrf token")
		}
		if !obs.rebootCalled {
			t.Fatal("POST reboot.cgi?reboot was never called")
		}
		if obs.rebootCookie == "" {
			t.Fatal("session cookie set by login.cgi was not present on the reboot POST")
		}
		if obs.rebootToken != "TOKENabc123" {
			t.Fatalf("reboot POST csrf_token = %q, want %q", obs.rebootToken, "TOKENabc123")
		}
		if !strings.Contains(obs.rebootCT, "application/x-www-form-urlencoded") {
			t.Fatalf("reboot POST Content-Type = %q, want application/x-www-form-urlencoded", obs.rebootCT)
		}
	})

	t.Run("lsid cookie name is also recognized and forwarded", func(t *testing.T) {
		t.Parallel()

		srv, obs := newRouterServer(t, routerServerConfig{
			validUser:     "admin",
			validPass:     "secret",
			sessionCookie: dto.CookieNameLSID,
		})

		d := newTestDriver(t, srv.URL, "admin", "secret")
		if err := d.Reboot(t.Context()); err != nil {
			t.Fatalf("Reboot: %v", err)
		}
		if obs.rebootCookie == "" {
			t.Fatal("lsid session cookie was not forwarded to the reboot POST")
		}
	})

	t.Run("bad credentials rejected, no reboot attempted", func(t *testing.T) {
		t.Parallel()

		srv, obs := newRouterServer(t, routerServerConfig{
			validUser:     "admin",
			validPass:     "secret",
			sessionCookie: dto.CookieNameSID,
		})

		d := newTestDriver(t, srv.URL, "admin", "wrong-password")
		err := d.Reboot(t.Context())
		if err == nil {
			t.Fatal("Reboot: want error for bad credentials, got nil")
		}
		if !errors.Is(err, ErrLoginFailed) {
			t.Fatalf("Reboot error = %v, want it to wrap ErrLoginFailed", err)
		}
		if obs.getRebootCalled {
			t.Fatal("GET /reboot.cgi was called despite failed login")
		}
		if obs.rebootCalled {
			t.Fatal("reboot POST was called despite failed login")
		}
	})

	t.Run("firmware bounces the reboot POST to login → ErrRebootNotConfirmed", func(t *testing.T) {
		t.Parallel()

		srv, obs := newRouterServer(t, routerServerConfig{
			validUser:     "admin",
			validPass:     "secret",
			sessionCookie: dto.CookieNameSID,
			rejectReboot:  true,
		})

		d := newTestDriver(t, srv.URL, "admin", "secret")
		err := d.Reboot(t.Context())
		if err == nil {
			t.Fatal("Reboot: want error when reboot.cgi bounces to login, got nil")
		}
		if !errors.Is(err, ErrRebootNotConfirmed) {
			t.Fatalf("Reboot error = %v, want it to wrap ErrRebootNotConfirmed", err)
		}
		if !obs.rebootCalled {
			t.Fatal("reboot POST should have been attempted before the bounce was seen")
		}
	})

	t.Run("200 without the done-reboot marker → ErrRebootNotConfirmed", func(t *testing.T) {
		t.Parallel()

		srv, obs := newRouterServer(t, routerServerConfig{
			validUser:       "admin",
			validPass:       "secret",
			sessionCookie:   dto.CookieNameSID,
			emptyRebootBody: true,
		})

		d := newTestDriver(t, srv.URL, "admin", "secret")
		err := d.Reboot(t.Context())
		if err == nil {
			t.Fatal("Reboot: want error when a 200 body lacks the done-reboot marker, got nil")
		}
		if !errors.Is(err, ErrRebootNotConfirmed) {
			t.Fatalf("Reboot error = %v, want it to wrap ErrRebootNotConfirmed", err)
		}
		if !obs.rebootCalled {
			t.Fatal("reboot POST should have been attempted")
		}
	})

	t.Run("reboot page without a csrf token → error, no reboot POST", func(t *testing.T) {
		t.Parallel()

		srv, obs := newRouterServer(t, routerServerConfig{
			validUser:     "admin",
			validPass:     "secret",
			sessionCookie: dto.CookieNameSID,
			noTokenPage:   true,
		})

		d := newTestDriver(t, srv.URL, "admin", "secret")
		err := d.Reboot(t.Context())
		if err == nil {
			t.Fatal("Reboot: want error when the reboot page carries no csrf token, got nil")
		}
		if !errors.Is(err, dto.ErrCSRFTokenNotFound) {
			t.Fatalf("Reboot error = %v, want it to wrap dto.ErrCSRFTokenNotFound", err)
		}
		if obs.rebootCalled {
			t.Fatal("reboot POST must not fire when the csrf token could not be parsed")
		}
	})

	t.Run("reboot page GET rejected (non-200) → error, no reboot POST", func(t *testing.T) {
		t.Parallel()

		srv, obs := newRouterServer(t, routerServerConfig{
			validUser:     "admin",
			validPass:     "secret",
			sessionCookie: dto.CookieNameSID,
			rejectGet:     true,
		})

		d := newTestDriver(t, srv.URL, "admin", "secret")
		if err := d.Reboot(t.Context()); err == nil {
			t.Fatal("Reboot: want error when GET /reboot.cgi is bounced, got nil")
		}
		if obs.rebootCalled {
			t.Fatal("reboot POST must not fire when the token page could not be fetched")
		}
	})

	t.Run("trailing slash in router address does not double the path", func(t *testing.T) {
		t.Parallel()

		var gotPaths []string
		srv := newRouterServerRecordingPaths(t, &gotPaths)

		d := newTestDriver(t, srv.URL+"/", "admin", "secret")
		if err := d.Reboot(t.Context()); err != nil {
			t.Fatalf("Reboot: %v", err)
		}

		want := []string{"/login.cgi", "/reboot.cgi", "/reboot.cgi"}
		if len(gotPaths) != len(want) {
			t.Fatalf("recorded paths = %v, want %v", gotPaths, want)
		}
		for i, p := range want {
			if gotPaths[i] != p {
				t.Fatalf("request %d path = %q, want %q (no double slash)", i, gotPaths[i], p)
			}
		}
	})
}

func TestNokiaDriver_CheckCredentials(t *testing.T) {
	t.Parallel()

	t.Run("login ok returns nil", func(t *testing.T) {
		t.Parallel()

		srv, obs := newRouterServer(t, routerServerConfig{
			validUser:     "admin",
			validPass:     "secret",
			sessionCookie: dto.CookieNameSID,
		})

		d := newTestDriver(t, srv.URL, "admin", "secret")
		if err := d.CheckCredentials(t.Context()); err != nil {
			t.Fatalf("CheckCredentials() error = %v, want nil", err)
		}
		if obs.rebootCalled {
			t.Fatal("CheckCredentials called reboot.cgi — it must never reboot")
		}
	})

	t.Run("bad credentials returns ErrLoginFailed", func(t *testing.T) {
		t.Parallel()

		srv, obs := newRouterServer(t, routerServerConfig{
			validUser:     "admin",
			validPass:     "secret",
			sessionCookie: dto.CookieNameSID,
		})

		d := newTestDriver(t, srv.URL, "admin", "wrong")
		err := d.CheckCredentials(t.Context())
		if err == nil {
			t.Fatal("CheckCredentials() error = nil, want error for bad credentials")
		}
		if !errors.Is(err, ErrLoginFailed) {
			t.Fatalf("CheckCredentials() error = %v, want it to wrap ErrLoginFailed", err)
		}
		if obs.rebootCalled {
			t.Fatal("CheckCredentials called reboot.cgi — it must never reboot")
		}
	})

	t.Run("never calls reboot.cgi", func(t *testing.T) {
		t.Parallel()

		var rebootHit bool
		mux := http.NewServeMux()
		mux.HandleFunc("/login.cgi", func(w http.ResponseWriter, r *http.Request) {
			http.SetCookie(w, &http.Cookie{Name: dto.CookieNameSID, Value: "s", Path: "/"})
			w.WriteHeader(http.StatusOK)
		})
		mux.HandleFunc("/reboot.cgi", func(w http.ResponseWriter, _ *http.Request) {
			rebootHit = true
			w.WriteHeader(http.StatusOK)
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)

		d := newTestDriver(t, srv.URL, "admin", "secret")
		if err := d.CheckCredentials(t.Context()); err != nil {
			t.Fatalf("CheckCredentials() error = %v", err)
		}
		if rebootHit {
			t.Fatal("CheckCredentials called reboot.cgi — must never reboot")
		}
	})

	t.Run("session cookie is cleared from the jar after a successful check", func(t *testing.T) {
		t.Parallel()

		// NokiaDriver's "logout" is a local cookie-jar reset (not an HTTP
		// request) and cannot fail. This subtest verifies that the jar is
		// empty after CheckCredentials returns — a subsequent call would start
		// with no stale session cookies.
		srv, _ := newRouterServer(t, routerServerConfig{
			validUser:     "admin",
			validPass:     "secret",
			sessionCookie: dto.CookieNameSID,
		})

		d := newTestDriver(t, srv.URL, "admin", "secret")
		if err := d.CheckCredentials(t.Context()); err != nil {
			t.Fatalf("CheckCredentials() error = %v, want nil", err)
		}
		// after CheckCredentials the jar must be empty — logout (jar reset) worked.
		cookies := d.httpClient.Jar.Cookies(d.baseURL)
		for _, c := range cookies {
			if c.Name == dto.CookieNameSID || c.Name == dto.CookieNameLSID {
				t.Fatalf("session cookie %q still present after CheckCredentials; logout failed to clear jar", c.Name)
			}
		}
	})
}

// routerServerConfig parameterizes newRouterServer's fake Nokia endpoint
// behavior for each test scenario.
type routerServerConfig struct {
	validUser     string
	validPass     string
	sessionCookie string
	// csrfToken is the token GET /reboot.cgi embeds in its page and the value
	// the reboot POST must echo to be accepted; defaults to defaultTestCSRF.
	csrfToken string
	// rejectReboot makes POST reboot.cgi?reboot always 302-bounce to login,
	// simulating a firmware that rejects the token/session.
	rejectReboot bool
	// emptyRebootBody makes an accepted POST answer 200 with no body, simulating
	// a 200 that lacks the "done reboot" confirmation marker.
	emptyRebootBody bool
	// noTokenPage makes GET /reboot.cgi serve a page with no csrf_token input.
	noTokenPage bool
	// rejectGet makes GET /reboot.cgi 302-bounce, simulating a rejected session
	// before any token can be read.
	rejectGet bool
}

// routerServerObs records what the fake router observed, so a test can assert
// the driver drove the real two-step CSRF flow correctly.
type routerServerObs struct {
	getRebootCalled bool
	rebootCalled    bool
	rebootCookie    string // session cookie value seen on the reboot POST
	rebootToken     string // csrf_token value parsed from the reboot POST body
	rebootCT        string // Content-Type header seen on the reboot POST
}

// defaultTestCSRF is the csrf token newRouterServer serves when a config does
// not pin one explicitly.
const defaultTestCSRF = "TESTCSRF0001"

// newRouterServer starts an httptest.Server that fakes the Nokia login.cgi /
// reboot.cgi CSRF flow per cfg: login.cgi sets cfg.sessionCookie only when the
// posted name/pswd match; GET /reboot.cgi serves a page embedding the token;
// POST reboot.cgi?reboot accepts (200) only a body echoing that token, else
// 302-bounces to login. It returns the server and an observations pointer the
// caller inspects after Reboot/CheckCredentials.
func newRouterServer(t *testing.T, cfg routerServerConfig) (*httptest.Server, *routerServerObs) {
	t.Helper()

	obs := new(routerServerObs)
	token := cfg.csrfToken
	if token == "" {
		token = defaultTestCSRF
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/login.cgi", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, fmt.Sprintf("parse form: %v", err), http.StatusBadRequest)
			return
		}
		if r.PostForm.Get("name") == cfg.validUser && r.PostForm.Get("pswd") == cfg.validPass {
			http.SetCookie(w, &http.Cookie{Name: cfg.sessionCookie, Value: "test-session", Path: "/"})
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/reboot.cgi", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			obs.getRebootCalled = true
			if cfg.rejectGet {
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}
			if cfg.noTokenPage {
				fmt.Fprint(w, "<html><body>no token here</body></html>")
				return
			}
			fmt.Fprintf(w, `<html><body><form></form><script>$("form").prepend('<input type="hidden" name="csrf_token" value="%s" />');</script></body></html>`, token)
		case http.MethodPost:
			obs.rebootCalled = true
			obs.rebootCT = r.Header.Get("Content-Type")
			if c, err := r.Cookie(cfg.sessionCookie); err == nil {
				obs.rebootCookie = c.Value
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read body", http.StatusBadRequest)
				return
			}
			if vals, err := url.ParseQuery(string(body)); err == nil {
				obs.rebootToken = vals.Get(dto.CSRFTokenField)
			}
			if cfg.rejectReboot || obs.rebootToken != token {
				http.Redirect(w, r, "/", http.StatusFound) // bounce to login
				return
			}
			w.WriteHeader(http.StatusOK)
			if !cfg.emptyRebootBody {
				fmt.Fprint(w, dto.RebootDoneMarker) // confirmed reboot marker
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, obs
}

// newRouterServerRecordingPaths starts a minimal fake router that always
// succeeds the login → token GET → reboot POST flow, recording the raw request
// path seen by each handler in order into *paths — used to assert URL
// construction avoids a double slash when ROUTER_ADDR carries a trailing slash.
func newRouterServerRecordingPaths(t *testing.T, paths *[]string) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/login.cgi", func(w http.ResponseWriter, r *http.Request) {
		*paths = append(*paths, r.URL.Path)
		http.SetCookie(w, &http.Cookie{Name: dto.CookieNameSID, Value: "test-session", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/reboot.cgi", func(w http.ResponseWriter, r *http.Request) {
		*paths = append(*paths, r.URL.Path)
		if r.Method == http.MethodGet {
			fmt.Fprintf(w, `<input type="hidden" name="csrf_token" value="%s" />`, defaultTestCSRF)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, dto.RebootDoneMarker)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newTestDriver builds a NokiaDriver against addr with a short timeout
// suited to httptest servers.
func newTestDriver(t *testing.T, addr, user, pass string) *NokiaDriver {
	t.Helper()

	d, err := NewNokiaDriver(addr, user, pass, 5*time.Second)
	if err != nil {
		t.Fatalf("NewNokiaDriver: %v", err)
	}
	return d
}
