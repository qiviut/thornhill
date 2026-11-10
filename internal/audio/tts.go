// Package audio implements the L1 rung of the audible-error ladder: plain
// OpenAI TTS, pre-baked for canonical phrases at startup so playing one is
// a disk read, dynamic for parameterized novelties.
package audio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
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
}

func New(apiKey, websocketURL, model, voice, dir string, log *slog.Logger) *TTS {
	return &TTS{APIKey: apiKey, BaseURL: websocketURL, Model: model, Voice: voice, Dir: dir, Log: log}
}

// Prebake synthesizes all canned phrases that are not already on disk.
// Failures are logged, not fatal: L2 earcons cover the gap.
func (t *TTS) Prebake(ctx context.Context) {
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		t.Log.Error("prebake dir", "err", err)
		return
	}
	for key, text := range Canned {
		path := filepath.Join(t.Dir, key+".wav")
		if _, err := os.Stat(path); err == nil {
			continue
		}
		data, err := t.Speak(ctx, text)
		if err != nil {
			t.Log.Warn("prebake failed", "key", key, "err", err)
			continue
		}
		if err := writePrebaked(path, data); err != nil {
			t.Log.Warn("prebake write failed", "key", key, "err", err)
			continue
		}
		t.Log.Info("prebaked", "key", key, "bytes", len(data))
	}
}

func writePrebaked(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tts-*.wav")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0o644); err == nil {
		_, err = f.Write(data)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Speak requests audio via Realtime and wraps its PCM16 output in a WAV file.
func (t *TTS) Speak(ctx context.Context, text string) ([]byte, error) {
	if strings.TrimSpace(text) == "" || len(text) > 4000 {
		return nil, fmt.Errorf("invalid speech text")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	u, err := url.Parse(t.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid realtime URL: %w", err)
	}
	q := u.Query()
	q.Set("model", t.Model)
	u.RawQuery = q.Encode()
	conn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + t.APIKey}}})
	if err != nil {
		return nil, fmt.Errorf("tts websocket: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	read := func() (map[string]json.RawMessage, error) {
		kind, raw, readErr := conn.Read(ctx)
		if readErr != nil {
			return nil, readErr
		}
		var event map[string]json.RawMessage
		if kind != websocket.MessageText || json.Unmarshal(raw, &event) != nil {
			return nil, fmt.Errorf("invalid realtime event")
		}
		return event, nil
	}
	field := func(event map[string]json.RawMessage, key string) string {
		var value string
		_ = json.Unmarshal(event[key], &value)
		return value
	}
	waitFor := func(want string) error {
		for {
			event, readErr := read()
			if readErr != nil {
				return readErr
			}
			typ := field(event, "type")
			if typ == "error" {
				return fmt.Errorf("realtime provider error")
			}
			if typ == want {
				return nil
			}
		}
	}
	send := func(event any) error {
		data, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			return marshalErr
		}
		return conn.Write(ctx, websocket.MessageText, data)
	}
	if err := waitFor("session.created"); err != nil {
		return nil, err
	}
	if err := send(map[string]any{"type": "session.update", "session": map[string]any{
		"type": "realtime", "output_modalities": []string{"audio"},
		"instructions": "Read the user's text verbatim. Do not add or change words.",
		"audio":        map[string]any{"output": map[string]any{"voice": t.Voice, "format": map[string]any{"type": "audio/pcm", "rate": 24000}}},
	}}); err != nil {
		return nil, err
	}
	if err := waitFor("session.updated"); err != nil {
		return nil, err
	}
	if err := send(map[string]any{"type": "conversation.item.create", "item": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}); err != nil {
		return nil, err
	}
	if err := send(map[string]any{"type": "response.create", "response": map[string]any{"output_modalities": []string{"audio"}}}); err != nil {
		return nil, err
	}
	var pcm bytes.Buffer
	var responseID, transcript string
	audioDone := false
	for {
		event, readErr := read()
		if readErr != nil {
			return nil, readErr
		}
		switch field(event, "type") {
		case "error":
			return nil, fmt.Errorf("realtime provider error")
		case "response.created":
			var response struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(event["response"], &response) != nil || response.ID == "" || responseID != "" {
				return nil, fmt.Errorf("invalid response identity")
			}
			responseID = response.ID
		case "response.output_audio.delta":
			if responseID == "" || field(event, "response_id") != responseID || audioDone {
				return nil, fmt.Errorf("unexpected audio response")
			}
			chunk, decodeErr := base64.StdEncoding.Strict().DecodeString(field(event, "delta"))
			if decodeErr != nil || len(chunk) == 0 || pcm.Len()+len(chunk) > 2<<20 {
				return nil, fmt.Errorf("invalid or oversized audio")
			}
			_, _ = pcm.Write(chunk)
		case "response.output_audio.done":
			if responseID == "" || field(event, "response_id") != responseID || audioDone {
				return nil, fmt.Errorf("unexpected audio completion")
			}
			audioDone = true
		case "response.output_audio_transcript.done":
			if responseID == "" || field(event, "response_id") != responseID || transcript != "" {
				return nil, fmt.Errorf("unexpected transcript")
			}
			transcript = field(event, "transcript")
		case "response.done":
			var response struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			}
			if json.Unmarshal(event["response"], &response) != nil || response.ID == "" || response.ID != responseID || response.Status != "completed" || !audioDone || pcm.Len() == 0 || pcm.Len()%2 != 0 || !strings.EqualFold(strings.Join(strings.Fields(transcript), " "), strings.Join(strings.Fields(text), " ")) {
				return nil, fmt.Errorf("incomplete or mismatched speech response")
			}
			return pcmWAV(pcm.Bytes()), nil
		}
	}
}

func pcmWAV(pcm []byte) []byte {
	var out bytes.Buffer
	out.WriteString("RIFF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(36+len(pcm)))
	out.WriteString("WAVEfmt ")
	for _, value := range []any{uint32(16), uint16(1), uint16(1), uint32(24000), uint32(48000), uint16(2), uint16(16)} {
		_ = binary.Write(&out, binary.LittleEndian, value)
	}
	out.WriteString("data")
	_ = binary.Write(&out, binary.LittleEndian, uint32(len(pcm)))
	out.Write(pcm)
	return out.Bytes()
}
