package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"thornhill/internal/buildinfo"
)

func TestRunHealthcheck(t *testing.T) {
	oldCommit := buildinfo.Commit
	buildinfo.Commit = "0123456789abcdef0123456789abcdef01234567"
	t.Cleanup(func() { buildinfo.Commit = oldCommit })

	tests := []struct {
		name       string
		statusCode int
		body       string
		wantErr    bool
	}{
		{
			name:       "healthy matching revision",
			statusCode: http.StatusOK,
			body:       fmt.Sprintf(`{"status":"ok","source_commit":%q,"versioned":true}`, buildinfo.Commit),
		},
		{
			name:       "wrong revision",
			statusCode: http.StatusOK,
			body:       `{"status":"ok","source_commit":"ffffffffffffffffffffffffffffffffffffffff","versioned":true}`,
			wantErr:    true,
		},
		{
			name:       "unversioned",
			statusCode: http.StatusOK,
			body:       `{"status":"unversioned","source_commit":"unknown","versioned":false}`,
			wantErr:    true,
		},
		{
			name:       "server unavailable",
			statusCode: http.StatusServiceUnavailable,
			body:       `{}`,
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			err := runHealthcheck(server.URL)
			if (err != nil) != tt.wantErr {
				t.Fatalf("runHealthcheck() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
	t.Run("healthy unversioned development build", func(t *testing.T) {
		buildinfo.Commit = "unknown"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":"unversioned","source_commit":"unknown","versioned":false}`))
		}))
		defer server.Close()
		if err := runHealthcheck(server.URL); err != nil {
			t.Fatalf("runHealthcheck() error = %v", err)
		}
	})
}
