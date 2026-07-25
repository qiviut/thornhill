package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Hermes posts hooks server-side with no Origin, so the tailnet remains its only
// perimeter. A browser is different: a third-party page the operator visits can
// reach a tailnet address, and a text/plain POST is a simple request needing no
// preflight. Without an origin check that page could inject bus events and grow
// the durable event log, so cross-origin browser writes must stay rejected.
func TestHermesHooksRejectCrossOriginBrowserPosts(t *testing.T) {
	tests := []struct {
		name       string
		origin     string
		wantStatus int
		wantCalled bool
	}{
		{name: "server-side caller without origin", origin: "", wantStatus: http.StatusNoContent, wantCalled: true},
		{name: "same-origin browser", origin: "http://desk.example", wantStatus: http.StatusNoContent, wantCalled: true},
		{name: "cross-origin browser", origin: "http://evil.example", wantStatus: http.StatusForbidden},
		{name: "cross-origin browser on the same host", origin: "https://desk.example", wantStatus: http.StatusForbidden},
		{name: "unparseable origin", origin: "not an origin", wantStatus: http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			g := pushTestGateway(&fakeStore{}, false)
			g.Hooks = func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}

			req := httptest.NewRequest(http.MethodPost, "http://desk.example/hooks/hermes",
				strings.NewReader(`{"type":"probe"}`))
			req.Host = "desk.example"
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			g.Routes().ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if called != tc.wantCalled {
				t.Fatalf("hook handler called = %t, want %t", called, tc.wantCalled)
			}
		})
	}
}
