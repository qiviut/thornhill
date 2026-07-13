package gateway

import (
	"crypto/sha256"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"thornhill/internal/config"
)

func FuzzValidCallID(f *testing.F) {
	f.Add("rtc_valid-ABC_123")
	f.Add("not-a-call")
	f.Add("rtc_")
	f.Fuzz(func(t *testing.T, callID string) {
		if len(callID) > 4096 {
			callID = callID[:4096]
		}
		if !validCallID(callID) {
			return
		}
		if !strings.HasPrefix(callID, "rtc_") || len(callID) <= len("rtc_") {
			t.Fatalf("accepted malformed call ID %q", callID)
		}
		for _, r := range callID[len("rtc_"):] {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				t.Fatalf("accepted call ID with unsafe rune %q in %q", r, callID)
			}
		}
	})
}

func FuzzOriginPolicy(f *testing.F) {
	f.Add("https://thornhill.example", "thornhill.example")
	f.Add("https://evil.example", "thornhill.example")
	f.Add("not a url", "thornhill.example")
	f.Fuzz(func(t *testing.T, origin, host string) {
		if len(origin) > 4096 || len(host) > 1024 {
			return
		}
		g := &Gateway{Cfg: &config.Config{}}
		r := httptest.NewRequest("GET", "http://thornhill.invalid/ws", nil)
		r.Host = host
		r.Header.Set("Origin", origin)
		allowed := g.originAllowed(r)
		if origin == "" && !allowed {
			t.Fatal("non-browser request without Origin was rejected")
		}

		sum := sha256.Sum256([]byte(origin + "\x00" + host))
		trustedHost := fmt.Sprintf("thornhill-%x.example", sum[:8])
		same := httptest.NewRequest("GET", "https://"+trustedHost+"/ws", nil)
		same.Host = trustedHost
		same.Header.Set("Origin", "https://"+trustedHost)
		if !g.originAllowed(same) {
			t.Fatal("well-formed same-origin request was rejected")
		}
		cross := httptest.NewRequest("GET", "https://"+trustedHost+"/ws", nil)
		cross.Host = trustedHost
		cross.Header.Set("Origin", fmt.Sprintf("https://evil-%x.example", sum[8:16]))
		if g.originAllowed(cross) {
			t.Fatal("well-formed cross-origin request was accepted with an empty allow-list")
		}
	})
}
