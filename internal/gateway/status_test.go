package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"thornhill/internal/buildinfo"
	"thornhill/internal/config"
)

func TestStatusReportsEmbeddedCommit(t *testing.T) {
	old := buildinfo.Commit
	buildinfo.Commit = "0123456789abcdef0123456789abcdef01234567"
	t.Cleanup(func() { buildinfo.Commit = old })

	g := &Gateway{
		Cfg:   &config.Config{StaticDir: t.TempDir(), PrebakeDir: t.TempDir()},
		Hooks: func(http.ResponseWriter, *http.Request) {},
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()
	g.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	var body struct {
		Status       string `json:"status"`
		SourceCommit string `json:"source_commit"`
		Versioned    bool   `json:"versioned"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || !body.Versioned || body.SourceCommit != buildinfo.Commit {
		t.Fatalf("status body = %+v", body)
	}
}
