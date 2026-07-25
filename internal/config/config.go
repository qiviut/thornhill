// Package config loads Thornhill's runtime configuration from the
// environment. Every knob has a sane default; only OPENAI_API_KEY and
// DATABASE_URL are required.
package config

import (
	"bytes"
	"crypto/ecdh"
	"encoding/base64"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// --- required ---
	OpenAIKey   string // OPENAI_API_KEY: the single key for realtime, TTS and summaries
	DatabaseURL string // DATABASE_URL: postgres://...

	// --- openai ---
	OpenAIBaseURL       string // OPENAI_BASE_URL, default https://api.openai.com
	OpenAIRealtimeWSURL string // OPENAI_REALTIME_WS_URL, default wss://api.openai.com/v1/realtime
	RealtimeModel       string // REALTIME_MODEL, default gpt-realtime-2.1
	Voice               string // REALTIME_VOICE, default marin
	TTSModel            string // TTS_MODEL, default gpt-4o-mini-tts. TODO(verify): newest speech model name.
	TTSVoice            string // TTS_VOICE, default alloy
	SummaryModel        string // SUMMARY_MODEL, default gpt-5.5 (swap for a cheaper variant if available)
	TranscribeModel     string // TRANSCRIBE_MODEL, default gpt-realtime-whisper. TODO(verify) against docs/vendor.
	SafetyID            string // SAFETY_IDENTIFIER, sent as OpenAI-Safety-Identifier (single user, static)

	// --- hermes ---
	HermesBaseURL     string        // HERMES_BASE_URL: OpenAI-compatible API server of the Hermes instance. Empty => stub worker.
	HermesAPIKey      string        // HERMES_API_KEY: optional bearer for the Hermes API server
	HermesModel       string        // HERMES_MODEL: model name passed through; Hermes decides. Default "default".
	ApprovalParkAfter time.Duration // APPROVAL_PARK_AFTER, default 15m: reclaim a silent approval run without deciding it

	// --- server ---
	ListenAddr string // LISTEN_ADDR, default :8787 (bind to tailscale iface or firewall on host)
	StaticDir  string // STATIC_DIR, default web/dist
	// AllowedOrigins is a comma-separated allow-list of browser origins for the
	// control WebSocket and SDP relay. The request host is always allowed.
	// Leave empty in production when the UI is served by Thornhill itself.
	AllowedOrigins []string // ALLOWED_ORIGINS, e.g. localhost:5173 for Vite dev
	PrebakeDir     string   // PREBAKE_DIR, default ./prebaked

	// --- optional Web Push ---
	PushVAPIDPublicKey  string // PUSH_VAPID_PUBLIC_KEY: URL-safe base64 P-256 public key
	PushVAPIDPrivateKey string // PUSH_VAPID_PRIVATE_KEY: URL-safe base64 P-256 private key
	PushVAPIDSubject    string // PUSH_VAPID_SUBJECT: mailto: or https: operator contact

	// --- session lifecycle timer ladder ---
	QuietAfter time.Duration // QUIET_AFTER, default 30s: model goes silent-listening
	ParkAfter  time.Duration // PARK_AFTER, default 10m: tear down the realtime call
	RolloverAt time.Duration // ROLLOVER_AT, default 57m: park+resume before the 60m hard cap

	// --- dispatch ---
	FakeJobSeconds int // FAKE_JOB_SECONDS, default 90: stub worker duration when HERMES_BASE_URL is empty

	// --- retention ---
	// EventRetention bounds only mechanical telemetry in event_log. Job outcomes
	// and operator authority decisions are retained indefinitely: that corpus is
	// how the capability of the dispatched agents is improved over time.
	EventRetention time.Duration // EVENT_RETENTION, default 720h

	// --- budget ---
	DailyBudgetUSD float64 // DAILY_BUDGET_USD, default 25: breaker for the voice leg (estimate)
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// getdur, getint and getfloat fail closed on a malformed value rather than
// silently substituting the default. A typo in a breaker threshold or a timer
// bound must not present itself as a working configuration.
func getdur(k string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration such as 90s or 15m: %w", k, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", k)
	}
	return d, nil
}

func getint(k string, def int) (int, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", k, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", k)
	}
	return n, nil
}

func getfloat(k string, def float64) (float64, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number: %w", k, err)
	}
	if f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%s must be a finite, non-negative number", k)
	}
	return f, nil
}

func splitCSV(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateProviderURL(name, raw string, websocket bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%s must be an absolute URL", name)
	}
	if u.User != nil || u.Fragment != "" {
		return fmt.Errorf("%s must not contain userinfo or a fragment", name)
	}
	if !websocket && u.RawQuery != "" {
		return fmt.Errorf("%s must not contain a query", name)
	}
	secure, insecure := "https", "http"
	if websocket {
		secure, insecure = "wss", "ws"
	}
	if u.Scheme == secure {
		return nil
	}
	if u.Scheme == insecure && loopbackHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("%s must use %s, except %s is allowed for an explicit loopback host", name, secure, insecure)
}

func realtimeURL(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/realtime"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func validatePushConfig(c *Config) error {
	configured := c.PushVAPIDPublicKey != "" || c.PushVAPIDPrivateKey != "" || c.PushVAPIDSubject != ""
	if !configured {
		return nil
	}
	if c.PushVAPIDPublicKey == "" || c.PushVAPIDPrivateKey == "" || c.PushVAPIDSubject == "" {
		return fmt.Errorf("PUSH_VAPID_PUBLIC_KEY, PUSH_VAPID_PRIVATE_KEY, and PUSH_VAPID_SUBJECT must be set together")
	}
	public, err := base64.RawURLEncoding.DecodeString(c.PushVAPIDPublicKey)
	if err != nil {
		return fmt.Errorf("PUSH_VAPID_PUBLIC_KEY must be unpadded URL-safe base64: %w", err)
	}
	if _, err := ecdh.P256().NewPublicKey(public); err != nil {
		return fmt.Errorf("PUSH_VAPID_PUBLIC_KEY is not a valid P-256 public key: %w", err)
	}
	private, err := base64.RawURLEncoding.DecodeString(c.PushVAPIDPrivateKey)
	if err != nil {
		return fmt.Errorf("PUSH_VAPID_PRIVATE_KEY must be unpadded URL-safe base64: %w", err)
	}
	privateKey, err := ecdh.P256().NewPrivateKey(private)
	if err != nil {
		return fmt.Errorf("PUSH_VAPID_PRIVATE_KEY is not a valid P-256 private key: %w", err)
	}
	if !bytes.Equal(privateKey.PublicKey().Bytes(), public) {
		return fmt.Errorf("PUSH_VAPID_PUBLIC_KEY and PUSH_VAPID_PRIVATE_KEY are not the same P-256 key pair")
	}
	subject, err := url.Parse(c.PushVAPIDSubject)
	if err != nil || (subject.Scheme != "mailto" && subject.Scheme != "https") {
		return fmt.Errorf("PUSH_VAPID_SUBJECT must be a mailto: or https: URI")
	}
	if subject.Scheme == "https" && subject.Host == "" {
		return fmt.Errorf("PUSH_VAPID_SUBJECT https URI must include a host")
	}
	if subject.Scheme == "mailto" && strings.TrimSpace(subject.Opaque+subject.Path) == "" {
		return fmt.Errorf("PUSH_VAPID_SUBJECT mailto URI must include an address")
	}
	return nil
}

func validateDatabasePassword() error {
	password, configured := os.LookupEnv("THORNHILL_DB_PASSWORD")
	if !configured {
		return nil // Bare go-run deployments may provide only DATABASE_URL.
	}
	if len(password) != 64 || strings.IndexFunc(password, func(r rune) bool {
		return !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f'))
	}) != -1 {
		return fmt.Errorf("THORNHILL_DB_PASSWORD must be exactly 64 lowercase hexadecimal characters")
	}
	return nil
}

func Load() (*Config, error) {
	c := &Config{
		OpenAIKey:           os.Getenv("OPENAI_API_KEY"),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		OpenAIBaseURL:       strings.TrimRight(getenv("OPENAI_BASE_URL", "https://api.openai.com"), "/"),
		OpenAIRealtimeWSURL: strings.TrimSpace(os.Getenv("OPENAI_REALTIME_WS_URL")),
		RealtimeModel:       getenv("REALTIME_MODEL", "gpt-realtime-2.1"),
		Voice:               getenv("REALTIME_VOICE", "marin"),
		TTSModel:            getenv("TTS_MODEL", "gpt-4o-mini-tts"),
		TTSVoice:            getenv("TTS_VOICE", "alloy"),
		SummaryModel:        getenv("SUMMARY_MODEL", "gpt-5.5"),
		TranscribeModel:     getenv("TRANSCRIBE_MODEL", "gpt-realtime-whisper"),
		SafetyID:            getenv("SAFETY_IDENTIFIER", "thornhill-admin"),
		HermesBaseURL:       strings.TrimRight(strings.TrimSpace(os.Getenv("HERMES_BASE_URL")), "/"),
		HermesAPIKey:        os.Getenv("HERMES_API_KEY"),
		HermesModel:         getenv("HERMES_MODEL", "default"),
		ListenAddr:          getenv("LISTEN_ADDR", ":8787"),
		StaticDir:           getenv("STATIC_DIR", "web/dist"),
		AllowedOrigins:      splitCSV(os.Getenv("ALLOWED_ORIGINS")),
		PrebakeDir:          getenv("PREBAKE_DIR", "prebaked"),
		PushVAPIDPublicKey:  strings.TrimSpace(os.Getenv("PUSH_VAPID_PUBLIC_KEY")),
		PushVAPIDPrivateKey: strings.TrimSpace(os.Getenv("PUSH_VAPID_PRIVATE_KEY")),
		PushVAPIDSubject:    strings.TrimSpace(os.Getenv("PUSH_VAPID_SUBJECT")),
	}
	for _, knob := range []struct {
		name   string
		target *time.Duration
		def    time.Duration
	}{
		{"APPROVAL_PARK_AFTER", &c.ApprovalParkAfter, 15 * time.Minute},
		{"QUIET_AFTER", &c.QuietAfter, 30 * time.Second},
		{"PARK_AFTER", &c.ParkAfter, 10 * time.Minute},
		{"ROLLOVER_AT", &c.RolloverAt, 57 * time.Minute},
		{"EVENT_RETENTION", &c.EventRetention, 30 * 24 * time.Hour},
	} {
		value, durErr := getdur(knob.name, knob.def)
		if durErr != nil {
			return nil, durErr
		}
		*knob.target = value
	}
	var err error
	if c.FakeJobSeconds, err = getint("FAKE_JOB_SECONDS", 90); err != nil {
		return nil, err
	}
	if c.DailyBudgetUSD, err = getfloat("DAILY_BUDGET_USD", 25); err != nil {
		return nil, err
	}
	if c.OpenAIRealtimeWSURL == "" {
		c.OpenAIRealtimeWSURL = realtimeURL(c.OpenAIBaseURL)
	}
	if err = validateDatabasePassword(); err != nil {
		return nil, err
	}
	if err = validateProviderURL("OPENAI_BASE_URL", c.OpenAIBaseURL, false); err != nil {
		return nil, err
	}
	if err = validateProviderURL("OPENAI_REALTIME_WS_URL", c.OpenAIRealtimeWSURL, true); err != nil {
		return nil, err
	}
	if c.HermesBaseURL != "" {
		if err = validateProviderURL("HERMES_BASE_URL", c.HermesBaseURL, false); err != nil {
			return nil, err
		}
	}
	if err = validatePushConfig(c); err != nil {
		return nil, err
	}
	if c.OpenAIKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY is required")
	}
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return c, nil
}
