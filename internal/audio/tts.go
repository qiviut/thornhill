// Package audio implements the L1 rung of the audible-error ladder: plain
// OpenAI TTS, pre-baked for canonical phrases at startup so playing one is
// a disk read, dynamic for parameterized novelties.
package audio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Canonical phrases. Keys are stable; the frontend fetches these on load
// and can replay them even mid-outage.
var Canned = map[string]string{
	"voice_lost":      "Voice connection lost. Reconnecting.",
	"voice_down":      "Voice service is unavailable. Jobs continue in the background.",
	"backend_error":   "Backend error. Check the board when you can.",
	"resume_ok":       "Voice link restored.",
	"approval_needed": "A background job needs your approval. Resume voice and I will explain.",
	"budget_tripped":  "Daily voice budget reached. Voice is paused; text and board still work.",
}

type TTS struct {
	APIKey  string
	BaseURL string
	Model   string
	Voice   string
	Dir     string
	Log     *slog.Logger
	HTTP    *http.Client
}

func New(apiKey, baseURL, model, voice, dir string, log *slog.Logger) *TTS {
	return &TTS{APIKey: apiKey, BaseURL: strings.TrimRight(baseURL, "/"), Model: model, Voice: voice, Dir: dir, Log: log,
		HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// Prebake synthesizes all canned phrases that are not already on disk.
// Failures are logged, not fatal: L2 earcons cover the gap.
func (t *TTS) Prebake(ctx context.Context) {
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		t.Log.Error("prebake dir", "err", err)
		return
	}
	for key, text := range Canned {
		path := filepath.Join(t.Dir, key+".mp3")
		if _, err := os.Stat(path); err == nil {
			continue
		}
		data, err := t.Speak(ctx, text)
		if err != nil {
			t.Log.Warn("prebake failed", "key", key, "err", err)
			continue
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Log.Warn("prebake write failed", "key", key, "err", err)
			continue
		}
		t.Log.Info("prebaked", "key", key, "bytes", len(data))
	}
}

// Speak synthesizes arbitrary text to mp3 bytes via POST /v1/audio/speech.
func (t *TTS) Speak(ctx context.Context, text string) ([]byte, error) {
	body, err := json.Marshal(map[string]any{
		"model":           t.Model,
		"voice":           t.Voice,
		"input":           text,
		"response_format": "mp3",
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.BaseURL+"/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.APIKey)
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("tts http %d: %s", resp.StatusCode, string(b))
	}
	return io.ReadAll(resp.Body)
}
