package config

import (
	"fmt"
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

func TestLoadRejectsMalformedDatabasePassword(t *testing.T) {
	for _, value := range []string{
		"",
		"abc",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdeF",
		"g123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	} {
		t.Run(fmt.Sprintf("length-%d", len(value)), func(t *testing.T) {
			t.Setenv("OPENAI_API_KEY", "test-key")
			t.Setenv("DATABASE_URL", "postgres://test:***@localhost/test")
			t.Setenv("THORNHILL_DB_PASSWORD", value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "THORNHILL_DB_PASSWORD") {
				t.Fatalf("Load() error = %v, want database password validation error", err)
			}
		})
	}
}

func TestLoadAcceptsDeploymentDatabasePassword(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("DATABASE_URL", "postgres://test:***@localhost/test")
	t.Setenv("THORNHILL_DB_PASSWORD", strings.Repeat("a", 64))
	if _, err := Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
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

// Every other value in this package fails closed on malformed input. The numeric
// and duration knobs must behave the same way: a typo in a breaker threshold or
// a timer bound silently becoming the default is a configuration that looks like
// it works and does not.
func TestMalformedNumericAndDurationKnobsFailClosed(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		contain string
	}{
		{name: "unparseable budget", key: "DAILY_BUDGET_USD", value: "25 usd", contain: "must be a number"},
		{name: "infinite budget", key: "DAILY_BUDGET_USD", value: "Inf", contain: "finite"},
		{name: "unparseable stub duration", key: "FAKE_JOB_SECONDS", value: "ninety", contain: "must be an integer"},
		{name: "negative stub duration", key: "FAKE_JOB_SECONDS", value: "-5", contain: "greater than zero"},
		{name: "unparseable timer", key: "PARK_AFTER", value: "10 minutes", contain: "must be a Go duration"},
		// A zero rollover would park and redial every call on the first tick.
		{name: "zero rollover", key: "ROLLOVER_AT", value: "0s", contain: "greater than zero"},
		{name: "zero quiet window", key: "QUIET_AFTER", value: "0", contain: "greater than zero"},
		{name: "zero approval threshold", key: "APPROVAL_PARK_AFTER", value: "0s", contain: "greater than zero"},
		{name: "negative retention", key: "EVENT_RETENTION", value: "-1h", contain: "greater than zero"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPENAI_API_KEY", "test-key")
			t.Setenv("DATABASE_URL", "postgres://localhost/thornhill")
			t.Setenv(tc.key, tc.value)
			cfg, err := Load()
			if err == nil {
				t.Fatalf("Load() accepted %s=%q as %+v", tc.key, tc.value, cfg)
			}
			if !strings.Contains(err.Error(), tc.contain) {
				t.Fatalf("Load() error = %v, want it to mention %q", err, tc.contain)
			}
		})
	}
}

func TestDefaultsApplyWhenKnobsAreUnset(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("DATABASE_URL", "postgres://localhost/thornhill")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.DailyBudgetUSD != 25 || cfg.FakeJobSeconds != 90 {
		t.Fatalf("budget/stub defaults = %v/%v", cfg.DailyBudgetUSD, cfg.FakeJobSeconds)
	}
	if cfg.RolloverAt != 57*time.Minute || cfg.EventRetention != 30*24*time.Hour {
		t.Fatalf("timer defaults = %v/%v", cfg.RolloverAt, cfg.EventRetention)
	}
}

// A deliberate zero budget must still be accepted: it is the documented way to
// stop admitting new calls without tearing the deployment down.
func TestZeroDailyBudgetIsAnIntentionalBreaker(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("DATABASE_URL", "postgres://localhost/thornhill")
	t.Setenv("DAILY_BUDGET_USD", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.DailyBudgetUSD != 0 {
		t.Fatalf("DailyBudgetUSD = %v, want 0", cfg.DailyBudgetUSD)
	}
}
