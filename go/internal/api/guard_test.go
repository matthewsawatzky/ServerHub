package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewGuardAllowedHosts(t *testing.T) {
	g := NewGuard("127.0.0.1", 7070)
	for _, host := range []string{
		"127.0.0.1",
		"127.0.0.1:7070",
		"localhost",
		"localhost:7070",
		"[::1]",
		"[::1]:7070",
	} {
		if !g.hostAllowed(host) {
			t.Errorf("host %q should be allowed", host)
		}
	}
	for _, host := range []string{"evil.example", "evil.example:7070", "", "127.0.0.1:9999"} {
		if g.hostAllowed(host) {
			t.Errorf("host %q should be rejected", host)
		}
	}
}

func TestNewGuardAddsConfiguredBind(t *testing.T) {
	g := NewGuard("100.64.0.5", 7070)
	if !g.hostAllowed("100.64.0.5:7070") {
		t.Fatal("configured bind address should be allowed")
	}
	if !g.hostAllowed("localhost:7070") {
		t.Fatal("loopback should stay allowed")
	}
}

func TestGuardMiddleware(t *testing.T) {
	g := NewGuard("127.0.0.1", 7070)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := g.Middleware(next)

	cases := []struct {
		name    string
		method  string
		host    string
		headers map[string]string
		want    int
	}{
		{
			name:   "GET with allowed host",
			method: http.MethodGet,
			host:   "127.0.0.1:7070",
			want:   http.StatusNoContent,
		},
		{
			name:   "GET with foreign host",
			method: http.MethodGet,
			host:   "evil.example",
			want:   http.StatusForbidden,
		},
		{
			name:    "POST without preflight marker",
			method:  http.MethodPost,
			host:    "127.0.0.1:7070",
			headers: map[string]string{"Origin": "http://127.0.0.1:7070"},
			want:    http.StatusForbidden,
		},
		{
			name:    "POST with foreign origin",
			method:  http.MethodPost,
			host:    "127.0.0.1:7070",
			headers: map[string]string{"Origin": "http://evil.example", "X-Requested-With": "XMLHttpRequest"},
			want:    http.StatusForbidden,
		},
		{
			name:    "POST with same-origin and marker",
			method:  http.MethodPost,
			host:    "127.0.0.1:7070",
			headers: map[string]string{"Origin": "http://127.0.0.1:7070", "X-Requested-With": "XMLHttpRequest"},
			want:    http.StatusNoContent,
		},
		{
			name:    "POST CLI-style with JSON content type",
			method:  http.MethodPost,
			host:    "localhost:7070",
			headers: map[string]string{"Content-Type": "application/json; charset=utf-8"},
			want:    http.StatusNoContent,
		},
		{
			name:    "DELETE with foreign referer",
			method:  http.MethodDelete,
			host:    "127.0.0.1:7070",
			headers: map[string]string{"Referer": "http://evil.example/page", "X-Requested-With": "XMLHttpRequest"},
			want:    http.StatusForbidden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://"+tc.host+"/api/servers", nil)
			r.Host = tc.host
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestSecurityHeaders(t *testing.T) {
	handler := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := w.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if csp := w.Header().Get("Content-Security-Policy"); csp == "" {
		t.Error("Content-Security-Policy is missing")
	}
}
