package proxy

import (
	"embed"
	"net/http"
)

// dashboardFS embeds the single-page developer dashboard. It is pure
// HTML/CSS/vanilla-JS with inline SVG and polls the same-origin /v1/metrics
// endpoint, so it needs no server-side configuration, no external assets, and no
// network access.
//
//go:embed dashboard/index.html
var dashboardFS embed.FS

// DashboardHandler serves the embedded developer dashboard. The page drives
// itself entirely from /v1/metrics, so this handler has no other dependency.
func DashboardHandler() http.Handler {
	body, _ := dashboardFS.ReadFile("dashboard/index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(body)
	})
}
