package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"thornhill/internal/config"
	"thornhill/internal/dummyopenai"
	"thornhill/internal/events"
)

type nullWriter struct{}

func (nullWriter) Write(p []byte) (int, error) { return len(p), nil }

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(nullWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type fakeStore struct{ usd float64 }

func (f fakeStore) GetSummary(context.Context, string) (string, time.Time, error) {
	return "", time.Time{}, nil
}
func (f fakeStore) SaveSummary(context.Context, string, string) error { return nil }
func (f fakeStore) AddUsage(context.Context, string, int64, int64, float64) error {
	return nil
}
func (f fakeStore) UsageTodayUSD(context.Context) (float64, error) { return f.usd, nil }

func newTestGateway(t *testing.T, upstream string, usd float64, launched *string) *Gateway {
	t.Helper()
	var mu sync.Mutex
	return &Gateway{
		Cfg: &config.Config{
			OpenAIKey: "sk-test", RealtimeModel: "gpt-realtime-2.1", Voice: "marin",
			SafetyID: "thornhill-admin", DailyBudgetUSD: 25,
			PrebakeDir: t.TempDir(), StaticDir: t.TempDir(),
		},
		Bus:      events.NewBus(nil, quietLog()),
		Store:    fakeStore{usd: usd},
		Hooks:    func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
		Log:      quietLog(),
		Upstream: upstream,
		deskLaunch: func(callID string) {
			mu.Lock()
			*launched = callID
			mu.Unlock()
		},
	}
}

func TestOfferRelayContract(t *testing.T) {
	var gotSDP, gotSession, gotAuth, gotSafety, gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("upstream expected multipart form: %v", err)
		}
		gotSDP = r.FormValue("sdp")
		gotSession = r.FormValue("session")
		gotAuth = r.Header.Get("Authorization")
		gotSafety = r.Header.Get("OpenAI-Safety-Identifier")
		w.Header().Set("Location", "/v1/realtime/calls/rtc_test123")
		w.Header().Set("Content-Type", "application/sdp")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("v=0\nanswer-sdp"))
	}))
	defer up.Close()

	var launched string
	g := newTestGateway(t, up.URL, 0, &launched)

	req := httptest.NewRequest(http.MethodPost, "/offer", strings.NewReader("v=0\noffer-sdp"))
	req.Header.Set("Content-Type", "application/sdp")
	w := httptest.NewRecorder()
	g.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/sdp" {
		t.Fatalf("content-type = %q", ct)
	}
	body, _ := io.ReadAll(w.Body)
	if string(body) != "v=0\nanswer-sdp" {
		t.Fatalf("answer not relayed verbatim: %q", body)
	}
	if gotPath != "/v1/realtime/calls" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotSDP != "v=0\noffer-sdp" {
		t.Fatalf("sdp field = %q", gotSDP)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotSafety != "thornhill-admin" {
		t.Fatalf("safety id = %q", gotSafety)
	}
	var sess struct {
		Type  string `json:"type"`
		Model string `json:"model"`
		Audio struct {
			Input struct {
				TurnDetection struct {
					Type              string `json:"type"`
					CreateResponse    bool   `json:"create_response"`
					InterruptResponse bool   `json:"interrupt_response"`
				} `json:"turn_detection"`
			} `json:"input"`
			Output struct {
				Voice string `json:"voice"`
			} `json:"output"`
		} `json:"audio"`
	}
	if err := json.Unmarshal([]byte(gotSession), &sess); err != nil {
		t.Fatalf("session field not JSON: %v (%q)", err, gotSession)
	}
	if sess.Type != "realtime" || sess.Model != "gpt-realtime-2.1" || sess.Audio.Output.Voice != "marin" {
		t.Fatalf("session config wrong: %+v", sess)
	}
	turn := sess.Audio.Input.TurnDetection
	if turn.Type != "semantic_vad" || turn.CreateResponse || turn.InterruptResponse {
		t.Fatalf("initial turn detection does not serialize Desk responses: %+v", turn)
	}
	if launched != "rtc_test123" {
		t.Fatalf("desk launched with call_id %q", launched)
	}
}

func TestOfferUsesConfiguredProviderBase(t *testing.T) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	token := "dummy_" + hex.EncodeToString(raw[:])
	provider := httptest.NewServer(dummyopenai.New(token).Handler())
	defer provider.Close()
	var launched string
	g := newTestGateway(t, "", 0, &launched)
	g.Cfg.OpenAIKey = token
	g.Cfg.OpenAIBaseURL = provider.URL

	req := httptest.NewRequest(http.MethodPost, "/offer", strings.NewReader("v=0\r\ns=random-"+hex.EncodeToString(raw[8:])+"\r\n"))
	w := httptest.NewRecorder()
	g.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.HasPrefix(launched, "rtc_dummy_") {
		t.Fatalf("configured provider call ID = %q", launched)
	}
}

func TestOfferBudgetBreaker(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called when budget is tripped")
	}))
	defer up.Close()

	var launched string
	g := newTestGateway(t, up.URL, 30 /* >= 25 budget */, &launched)

	req := httptest.NewRequest(http.MethodPost, "/offer", strings.NewReader("v=0\noffer"))
	w := httptest.NewRecorder()
	g.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if launched != "" {
		t.Fatal("desk must not launch past budget")
	}
}

func TestOfferRejectsEmptyBody(t *testing.T) {
	var launched string
	g := newTestGateway(t, "http://127.0.0.1:1", 0, &launched)
	req := httptest.NewRequest(http.MethodPost, "/offer", strings.NewReader("   "))
	w := httptest.NewRecorder()
	g.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestOfferRejectsUntrustedOrigin(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for an untrusted origin")
	}))
	defer up.Close()

	var launched string
	g := newTestGateway(t, up.URL, 0, &launched)
	req := httptest.NewRequest(http.MethodPost, "/offer", strings.NewReader("v=0\noffer"))
	req.Header.Set("Origin", "https://untrusted.example")
	w := httptest.NewRecorder()
	g.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestOfferRejectsInvalidUpstreamCallID(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/v1/realtime/calls/not-a-call-id")
		w.WriteHeader(http.StatusCreated)
	}))
	defer up.Close()

	var launched string
	g := newTestGateway(t, up.URL, 0, &launched)
	req := httptest.NewRequest(http.MethodPost, "/offer", strings.NewReader("v=0\noffer"))
	w := httptest.NewRecorder()
	g.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if launched != "" {
		t.Fatal("desk must not launch with an invalid call id")
	}
}
