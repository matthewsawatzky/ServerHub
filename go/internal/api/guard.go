package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Guard enforces the per-request host/origin/preflight checks (audit V4):
//
//   - Host allowlist on every route — mitigates DNS rebinding and any other
//     non-expected Host reaching the API (curl -H "Host: evil.example").
//   - Origin/Referer allowlist on every mutating request — mitigates CSRF.
//   - "Force preflight" on mutating requests: a Content-Type of
//     application/json or an X-Requested-With header is required, so the
//     body-less "simple" cross-site POSTs (reboot/shutdown/quick actions)
//     can never be carried out without a preflight, and preflights fail
//     (no CORS headers are ever emitted).
type Guard struct {
	hosts map[string]struct{}
}

// NewGuard builds the host allowlist from the configured bind address:
// the bind itself (when set) plus loopback variants, each with and without
// the web port.
func NewGuard(bind string, port int) *Guard {
	g := &Guard{hosts: make(map[string]struct{})}
	base := []string{"127.0.0.1", "localhost", "::1"}
	if bind != "" && bind != "0.0.0.0" && bind != "::" && bind != "*" {
		dup := false
		for _, h := range base {
			if strings.EqualFold(h, bind) {
				dup = true
			}
		}
		if !dup {
			base = append(base, bind)
		}
	}
	p := strconv.Itoa(port)
	for _, h := range base {
		if strings.Contains(h, ":") { // bare IPv6 → bracketed form in Host/Origin
			b := "[" + h + "]"
			g.hosts[b] = struct{}{}
			g.hosts[b+":"+p] = struct{}{}
			continue
		}
		g.hosts[h] = struct{}{}
		g.hosts[h+":"+p] = struct{}{}
	}
	return g
}

func (g *Guard) hostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	_, ok := g.hosts[host]
	return ok
}

// originAllowed parses an Origin or Referer URL and checks its host.
func (g *Guard) originAllowed(originURL string) bool {
	u, err := url.Parse(originURL)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return g.hostAllowed(u.Host) || g.hostAllowed(u.Hostname())
}

// Middleware returns the wrapping handler.
func (g *Guard) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.hostAllowed(r.Host) {
			http.Error(w, "invalid Host header", http.StatusForbidden)
			return
		}
		if isMutating(r.Method) {
			// A cross-site request carries Origin (POST always does, even in
			// no-cors mode). If Origin/Referer is present but foreign — reject.
			if !g.requestOriginAllowed(r) {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
			// Non-browser clients (no Origin/Referer at all) must still send
			// the marker header so simple cross-site form/fetch posts cannot
			// sneak past either check.
			if !preflightMarkerPresent(r) {
				http.Error(w, "preflight header (Content-Type: application/json or X-Requested-With) required", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (g *Guard) requestOriginAllowed(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		return g.originAllowed(origin)
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		return g.originAllowed(ref)
	}
	return true // no Origin/Referer (CLI/curl) — preflight marker still applies
}

func preflightMarkerPresent(r *http.Request) bool {
	if equalFoldTrim(r.Header.Get("X-Requested-With"), "XMLHttpRequest") {
		return true
	}
	ct := r.Header.Get("Content-Type")
	return equalFoldTrim(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]), "application/json")
}

func equalFoldTrim(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), b)
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}
