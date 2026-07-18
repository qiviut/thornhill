package gateway

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"thornhill/internal/config"
	"thornhill/internal/store"
)

type pushTestStore struct {
	fakeStore
	saved   []store.PushSubscription
	deleted []string
}

func (s *pushTestStore) UpsertPushSubscription(_ context.Context, sub store.PushSubscription) error {
	s.saved = append(s.saved, sub)
	return nil
}
func (s *pushTestStore) DeletePushSubscription(_ context.Context, endpoint string) error {
	s.deleted = append(s.deleted, endpoint)
	return nil
}

func pushTestGateway(st Store, enabled bool) *Gateway {
	cfg := &config.Config{StaticDir: tTempStaticDir(), PrebakeDir: tTempStaticDir()}
	if enabled {
		cfg.PushVAPIDPublicKey = "public-key"
		cfg.PushVAPIDPrivateKey = "private-key-must-never-be-returned"
	}
	return &Gateway{Cfg: cfg, Store: st, Hooks: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// These handlers do not touch static assets; a stable nonexistent directory is
// enough and keeps this helper independent of testing.T.
func tTempStaticDir() string { return "/nonexistent-thornhill-test-assets" }

func TestPushConfigIsDisabledByDefaultAndNeverExposesPrivateKey(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		g := pushTestGateway(&pushTestStore{}, enabled)
		req := httptest.NewRequest(http.MethodGet, "https://thornhill.example/api/push/config", nil)
		rec := httptest.NewRecorder()
		g.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("enabled=%v status=%d", enabled, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "private-key") {
			t.Fatalf("private key exposed: %s", rec.Body.String())
		}
		var body struct {
			Enabled   bool   `json:"enabled"`
			PublicKey string `json:"public_key"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Enabled != enabled || (enabled && body.PublicKey != "public-key") || (!enabled && body.PublicKey != "") {
			t.Fatalf("enabled=%v body=%+v", enabled, body)
		}
	}
}

func TestPushSubscriptionRequiresSameOriginAndValidKeys(t *testing.T) {
	privateKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p256dh := base64.RawURLEncoding.EncodeToString(privateKey.PublicKey().Bytes())
	authBytes := make([]byte, 16)
	if _, err := rand.Read(authBytes); err != nil {
		t.Fatal(err)
	}
	auth := base64.RawURLEncoding.EncodeToString(authBytes)
	body := `{"endpoint":"https://push.example.test/capability","keys":{"p256dh":"` + p256dh + `","auth":"` + auth + `"}}`

	for _, tc := range []struct {
		name   string
		origin string
		body   string
		want   int
	}{
		{name: "same origin", origin: "https://thornhill.example", body: body, want: http.StatusNoContent},
		{name: "missing origin", body: body, want: http.StatusForbidden},
		{name: "cross origin", origin: "https://evil.example", body: body, want: http.StatusForbidden},
		{name: "plaintext endpoint", origin: "https://thornhill.example", body: strings.Replace(body, "https://push.example.test", "http://push.example.test", 1), want: http.StatusBadRequest},
		{name: "loopback endpoint", origin: "https://thornhill.example", body: strings.Replace(body, "push.example.test", "127.0.0.1", 1), want: http.StatusBadRequest},
		{name: "tailnet endpoint", origin: "https://thornhill.example", body: strings.Replace(body, "push.example.test", "100.100.100.100", 1), want: http.StatusBadRequest},
		{name: "localhost endpoint", origin: "https://thornhill.example", body: strings.Replace(body, "push.example.test", "notify.localhost", 1), want: http.StatusBadRequest},
		{name: "invalid key", origin: "https://thornhill.example", body: `{"endpoint":"https://push.example.test/capability","keys":{"p256dh":"bad","auth":"bad"}}`, want: http.StatusBadRequest},
		{name: "unknown browser field", origin: "https://thornhill.example", body: strings.TrimSuffix(body, "}") + `,"expirationTime":null}`, want: http.StatusBadRequest},
		{name: "trailing JSON", origin: "https://thornhill.example", body: body + `{}`, want: http.StatusBadRequest},
		{name: "oversized body", origin: "https://thornhill.example", body: body + strings.Repeat(" ", 17<<10), want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &pushTestStore{}
			g := pushTestGateway(st, true)
			req := httptest.NewRequest(http.MethodPost, "https://thornhill.example/api/push/subscriptions", strings.NewReader(tc.body))
			req.Host = "thornhill.example"
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			g.Routes().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if (tc.want == http.StatusNoContent) != (len(st.saved) == 1) {
				t.Fatalf("saved=%d status=%d", len(st.saved), rec.Code)
			}
		})
	}
}

func TestPushSubscriptionDeleteStoresOnlyEndpoint(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%v", enabled), func(t *testing.T) {
			st := &pushTestStore{}
			g := pushTestGateway(st, enabled)
			req := httptest.NewRequest(http.MethodDelete, "https://thornhill.example/api/push/subscriptions",
				strings.NewReader(`{"endpoint":"https://push.example.test/capability"}`))
			req.Host = "thornhill.example"
			req.Header.Set("Origin", "https://thornhill.example")
			rec := httptest.NewRecorder()
			g.Routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent || len(st.deleted) != 1 || st.deleted[0] != "https://push.example.test/capability" {
				t.Fatalf("status=%d deleted=%v body=%s", rec.Code, st.deleted, rec.Body.String())
			}
		})
	}
}

func TestPushSubscriptionIsUnavailableWhenDisabled(t *testing.T) {
	g := pushTestGateway(&pushTestStore{}, false)
	req := httptest.NewRequest(http.MethodPost, "https://thornhill.example/api/push/subscriptions", strings.NewReader(`{}`))
	req.Host = "thornhill.example"
	req.Header.Set("Origin", "https://thornhill.example")
	rec := httptest.NewRecorder()
	g.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
