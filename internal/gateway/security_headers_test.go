package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every response carries the policy, including error and API responses, because
// the header set is applied by the outermost handler rather than per route.
func TestSecurityHeadersAreAppliedToEveryRoute(t *testing.T) {
	g := pushTestGateway(&fakeStore{}, false)
	for _, path := range []string{"/", "/index.html", "/api/status", "/api/push/config", "/missing-asset"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://desk.example"+path, nil)
			req.Host = "desk.example"
			rec := httptest.NewRecorder()
			g.Routes().ServeHTTP(rec, req)

			for header, want := range map[string]string{
				"Content-Security-Policy": contentSecurityPolicy,
				"X-Content-Type-Options":  "nosniff",
				"Referrer-Policy":         "no-referrer",
			} {
				if got := rec.Header().Get(header); got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
		})
	}
}

// The UI is entirely same-origin, so the policy must not need an escape hatch.
// Any future need for 'unsafe-inline' or a remote host is a design change worth
// noticing here rather than a header edit: it would mean the page began executing
// or fetching something that is not this repository's own build output.
func TestContentSecurityPolicyAllowsNoInlineOrRemoteSources(t *testing.T) {
	for _, forbidden := range []string{"unsafe-inline", "unsafe-eval", "http://", "https://", "*"} {
		if strings.Contains(contentSecurityPolicy, forbidden) {
			t.Errorf("policy contains %q: %s", forbidden, contentSecurityPolicy)
		}
	}
	for _, required := range []string{
		"default-src 'self'",
		"script-src 'self'",
		// Live call audio is a MediaStream via srcObject, which no fetch directive
		// governs; blob: covers only locally created media URLs.
		"media-src 'self' blob:",
		// Same-origin control WebSocket and SDP relay.
		"connect-src 'self'",
		// Clickjacking the ring or an approval control.
		"frame-ancestors 'none'",
		"object-src 'none'",
		"base-uri 'none'",
	} {
		if !strings.Contains(contentSecurityPolicy, required) {
			t.Errorf("policy is missing %q: %s", required, contentSecurityPolicy)
		}
	}
}
