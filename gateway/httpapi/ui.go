package httpapi

import (
	_ "embed"
	"log"
	"net/http"
)

// rootPath is the second route (beside pingPath) that withAuth exempts from
// the shared-token check: the dashboard HTML carries no secret and must load
// before nginx injects any token (it injects only on /api/*). Both the route
// registration in register and the exemption in withAuth reference this one
// constant so a rename can't silently gate the page or ungate a real route.
const rootPath = "/"

// dashboardHTML is the self-contained, dependency-free status page served at
// rootPath. It is embedded into the binary so the UI version always equals the
// daemon (and thus API) version — no page/contract skew and no hand-copied
// /var/www asset. It talks to the JSON API over relative /api/v1/* URLs, so it
// works unchanged whether reached directly on the LAN or through the nginx
// edge.
//
//go:embed web/index.html
var dashboardHTML []byte

// handleDashboard serves the embedded dashboard at rootPath with a
// revalidate-always cache directive, so a freshly deployed UI is visible
// without a hard refresh (the page is tiny and same-versioned with the
// binary). It touches no dependency and holds no secret, mirroring handlePing.
func handleDashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if _, err := w.Write(dashboardHTML); err != nil {
		log.Printf("httpapi: write dashboard: %v", err)
	}
}
