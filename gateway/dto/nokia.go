package dto

import (
	"errors"
	"net/url"
	"regexp"
)

// CookieNameSID and CookieNameLSID are the two session-cookie names the
// Nokia web UI is known to use (README "Router compatibility"). They are
// only used to recognize a successful login; the cookiejar forwards
// whichever one was actually set, so neither name is hardcoded into the
// request path.
const (
	CookieNameSID  = "sid"
	CookieNameLSID = "lsid"
)

// CSRFTokenField is the form-field (and hidden-input) name the Nokia web UI
// uses to carry the per-request CSRF token that reboot.cgi validates. The
// firmware regenerates the token on every GET /reboot.cgi and rejects any
// reboot POST that does not echo the current one (README "Router
// compatibility").
const CSRFTokenField = "csrf_token"

// RebootDoneMarker is the body a confirmed reboot returns: reboot.cgi?reboot
// answers 200 with "done reboot" (verified against firmware HJHL16 — a real
// reboot drops the WAN moments later). A rejected request (stale/missing CSRF
// token) instead redirects to the login page and never contains this marker.
const RebootDoneMarker = "done reboot"

// ErrCSRFTokenNotFound reports that a reboot.cgi page carried no csrf_token
// hidden input — e.g. the router bounced the GET to the login page for an
// expired session, or the firmware changed its markup.
var ErrCSRFTokenNotFound = errors.New("dto: csrf_token not found in reboot page")

// NokiaLoginForm is the login.cgi POST body (README "Router compatibility"):
// name/pswd form fields, the only documented login contract for the Nokia
// G-240W-A web UI.
type NokiaLoginForm struct {
	Name string
	Pswd string
}

// Encode renders f as the application/x-www-form-urlencoded body login.cgi
// expects.
func (f NokiaLoginForm) Encode() string {
	return url.Values{"name": {f.Name}, "pswd": {f.Pswd}}.Encode()
}

// EncodeRebootBody renders the application/x-www-form-urlencoded body for the
// reboot.cgi?reboot POST. The literal "data" prefix mirrors the router's own
// Reboot button, whose click handler POSTs data:'data'; the firmware's
// ajaxSend prefilter then appends &csrf_token=<token>, so this is the exact
// body a real browser sends. The extra "data" key is inert (the firmware's
// own empty-POST path accepts a bare csrf_token=…), but it is kept for
// byte-fidelity to the captured request — do not invent bytes.
func EncodeRebootBody(token string) string {
	return "data&" + CSRFTokenField + "=" + url.QueryEscape(token)
}

// ParseCSRFToken extracts the per-request CSRF token embedded in a reboot.cgi
// page — a hidden <input name="csrf_token" value="…">, regenerated on every
// GET. It returns ErrCSRFTokenNotFound when no such token is present, which a
// caller treats as a failed reboot rather than proceeding with an empty token.
func ParseCSRFToken(page []byte) (string, error) {
	m := csrfTokenPattern.FindSubmatch(page)
	if m == nil {
		return "", ErrCSRFTokenNotFound
	}
	return string(m[1]), nil
}

// csrfTokenPattern matches the hidden csrf_token input the reboot page embeds,
// e.g. `name="csrf_token" value="aSrAmhUSmUfCdWgZ"`. It deliberately anchors on
// the name/value input shape so it does not also match the bare csrf_token=…
// literals in the page's inline JavaScript.
var csrfTokenPattern = regexp.MustCompile(`name="` + CSRFTokenField + `"[^>]*value="([^"]+)"`)
