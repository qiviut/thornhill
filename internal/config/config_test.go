package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

func TestLoadParsesAllowedOrigins(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("DATABASE_URL", "postgres://thornhill:thornhill@localhost:5432/thornhill?sslmode=disable")
	t.Setenv("ALLOWED_ORIGINS", " localhost:5173, https://dev.example.test , ")
	t.Setenv("OPENAI_BASE_URL", "http://127.0.0.1:49123/")
	t.Setenv("OPENAI_REALTIME_WS_URL", "ws://127.0.0.1:49123/v1/realtime")
	t.Setenv("APPROVAL_PARK_AFTER", "45s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"localhost:5173", "https://dev.example.test"}
	if !reflect.DeepEqual(cfg.AllowedOrigins, want) {
		t.Fatalf("AllowedOrigins = %#v, want %#v", cfg.AllowedOrigins, want)
	}
	if cfg.OpenAIBaseURL != "http://127.0.0.1:49123" || cfg.OpenAIRealtimeWSURL != "ws://127.0.0.1:49123/v1/realtime" {
		t.Fatalf("OpenAI endpoints = %q / %q", cfg.OpenAIBaseURL, cfg.OpenAIRealtimeWSURL)
	}
	if cfg.ApprovalParkAfter != 45*time.Second {
		t.Fatalf("ApprovalParkAfter = %s, want 45s", cfg.ApprovalParkAfter)
	}
}

func TestLoadDerivesRealtimeEndpointFromProviderBase(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("DATABASE_URL", "postgres://test:test@localhost/test")
	t.Setenv("OPENAI_BASE_URL", "http://[::1]:49124/provider/")
	t.Setenv("OPENAI_REALTIME_WS_URL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAIRealtimeWSURL != "ws://[::1]:49124/provider/v1/realtime" {
		t.Fatalf("derived Realtime endpoint = %q", cfg.OpenAIRealtimeWSURL)
	}
}

func TestLoadRejectsUnsafeProviderEndpoints(t *testing.T) {
	tests := []struct {
		name   string
		base   string
		ws     string
		hermes string
		want   string
	}{
		{name: "plaintext remote HTTP", base: "http://models.example.test", ws: "wss://models.example.test/v1/realtime", want: "OPENAI_BASE_URL"},
		{name: "plaintext remote WebSocket", base: "https://models.example.test", ws: "ws://models.example.test/v1/realtime", want: "OPENAI_REALTIME_WS_URL"},
		{name: "embedded credentials", base: "https://user:pass@models.example.test", ws: "wss://models.example.test/v1/realtime", want: "userinfo"},
		{name: "HTTP fragment", base: "https://models.example.test/#leak", ws: "wss://models.example.test/v1/realtime", want: "fragment"},
		{name: "HTTP query", base: "https://models.example.test?tenant=x", ws: "wss://models.example.test/v1/realtime", want: "query"},
		{name: "WebSocket fragment", base: "https://models.example.test", ws: "wss://models.example.test/v1/realtime#leak", want: "fragment"},
		{name: "plaintext remote Hermes", base: "https://models.example.test", ws: "wss://models.example.test/v1/realtime", hermes: "http://hermes.example.test", want: "HERMES_BASE_URL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPENAI_API_KEY", "test-key")
			t.Setenv("DATABASE_URL", "postgres://test:test@localhost/test")
			t.Setenv("OPENAI_BASE_URL", tc.base)
			t.Setenv("OPENAI_REALTIME_WS_URL", tc.ws)
			t.Setenv("HERMES_BASE_URL", tc.hermes)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestLoadRejectsNonPositiveApprovalParkingThreshold(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("DATABASE_URL", "postgres://test:***@localhost/test")
	t.Setenv("APPROVAL_PARK_AFTER", "0s")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "APPROVAL_PARK_AFTER") {
		t.Fatalf("Load() error = %v, want approval parking threshold error", err)
	}
}

func TestLoadAcceptsCompleteVAPIDConfiguration(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("DATABASE_URL", "postgres://test:***@localhost/test")
	privateKey, publicKey, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUSH_VAPID_PUBLIC_KEY", publicKey)
	t.Setenv("PUSH_VAPID_PRIVATE_KEY", privateKey)
	t.Setenv("PUSH_VAPID_SUBJECT", "mailto:operator@example.test")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PushVAPIDPublicKey != publicKey || cfg.PushVAPIDPrivateKey != privateKey {
		t.Fatal("VAPID keys were not loaded exactly")
	}
}

func TestLoadRejectsPartialOrMalformedVAPIDConfiguration(t *testing.T) {
	privateKey, publicKey, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	_, otherPublicKey, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		public  string
		private string
		subject string
	}{
		{name: "partial", public: "only-one-value"},
		{name: "malformed keys", public: "not-base64", private: "not-base64", subject: "mailto:operator@example.test"},
		{name: "mismatched key pair", public: otherPublicKey, private: privateKey, subject: "mailto:operator@example.test"},
		{name: "unsafe subject", public: publicKey, private: privateKey, subject: "javascript:alert(1)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPENAI_API_KEY", "test-key")
			t.Setenv("DATABASE_URL", "postgres://test:***@localhost/test")
			t.Setenv("PUSH_VAPID_PUBLIC_KEY", tc.public)
			t.Setenv("PUSH_VAPID_PRIVATE_KEY", tc.private)
			t.Setenv("PUSH_VAPID_SUBJECT", tc.subject)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PUSH_VAPID") {
				t.Fatalf("Load() error = %v, want PUSH_VAPID validation error", err)
			}
		})
	}
}
