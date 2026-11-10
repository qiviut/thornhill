package audio

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"thornhill/internal/dummyopenai"
)

func TestSpeakUsesConfiguredProvider(t *testing.T) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	token := "dummy_" + hex.EncodeToString(raw[:])
	server := httptest.NewServer(dummyopenai.New(token).Handler())
	defer server.Close()
	tts := New(token, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/realtime", "dummy-speech", "alloy", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	got, err := tts.Speak(context.Background(), "random-"+hex.EncodeToString(raw[8:]))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 48 || string(got[:4]) != "RIFF" || string(got[8:12]) != "WAVE" || binary.LittleEndian.Uint32(got[4:8]) != 40 || binary.LittleEndian.Uint16(got[22:24]) != 1 || binary.LittleEndian.Uint32(got[24:28]) != 24000 || binary.LittleEndian.Uint16(got[34:36]) != 16 || binary.LittleEndian.Uint32(got[40:44]) != 4 {
		t.Fatalf("speech payload = %q", got)
	}
}

func TestSpeakRejectsIncompleteOrUntrustedResponses(t *testing.T) {
	created := map[string]any{"type": "response.created", "response": map[string]any{"id": "r1"}}
	delta := map[string]any{"type": "response.output_audio.delta", "response_id": "r1", "delta": "AAAAAA=="}
	audioDone := map[string]any{"type": "response.output_audio.done", "response_id": "r1"}
	transcript := map[string]any{"type": "response.output_audio_transcript.done", "response_id": "r1", "transcript": "hello"}
	done := map[string]any{"type": "response.done", "response": map[string]any{"id": "r1", "status": "completed"}}
	large := map[string]any{"type": "response.output_audio.delta", "response_id": "r1", "delta": strings.Repeat("AAAA", 175000)}
	tests := map[string][]map[string]any{
		"provider error":           {{"type": "error"}},
		"invalid base64":           {created, {"type": "response.output_audio.delta", "response_id": "r1", "delta": "!!!"}},
		"wrong response":           {created, {"type": "response.output_audio.delta", "response_id": "other", "delta": "AAAAAA=="}},
		"missing audio completion": {created, delta, transcript, done},
		"empty audio":              {created, audioDone, transcript, done},
		"missing transcript":       {created, delta, audioDone, done},
		"changed words":            {created, delta, audioDone, {"type": "response.output_audio_transcript.done", "response_id": "r1", "transcript": "goodbye"}, done},
		"cancelled":                {created, delta, audioDone, transcript, {"type": "response.done", "response": map[string]any{"id": "r1", "status": "cancelled"}}},
		"closed early":             {created, delta},
		"oversized audio":          {created, large, large, large, large},
	}
	for name, events := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer dummy-token" || r.URL.Query().Get("model") != "dummy-model" {
					t.Error("wrong authentication or model")
				}
				c, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer c.CloseNow()
				ctx := r.Context()
				send := func(v any) bool { b, _ := json.Marshal(v); return c.Write(ctx, websocket.MessageText, b) == nil }
				if !send(map[string]any{"type": "session.created"}) {
					return
				}
				if _, _, err = c.Read(ctx); err != nil {
					return
				}
				if !send(map[string]any{"type": "session.updated"}) {
					return
				}
				for range 2 {
					if _, _, err = c.Read(ctx); err != nil {
						return
					}
				}
				for _, event := range events {
					if !send(event) {
						return
					}
				}
			}))
			defer server.Close()
			tts := New("dummy-token", "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/realtime", "dummy-model", "alloy", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := tts.Speak(ctx, "hello"); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}

func TestSpeakHonorsCancellationAndAuthentication(t *testing.T) {
	server := httptest.NewServer(dummyopenai.New("expected").Handler())
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := New("wrong", endpoint, "dummy", "alloy", t.TempDir(), logger).Speak(context.Background(), "hello"); err == nil {
		t.Fatal("accepted invalid provider authentication")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New("expected", endpoint, "dummy", "alloy", t.TempDir(), logger).Speak(ctx, "hello"); err == nil {
		t.Fatal("accepted canceled request")
	}
}

func TestSpeakHonorsResponseDeadline(t *testing.T) {
	responseStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		send := func(v any) bool {
			data, marshalErr := json.Marshal(v)
			return marshalErr == nil && conn.Write(ctx, websocket.MessageText, data) == nil
		}
		if !send(map[string]any{"type": "session.created"}) {
			return
		}
		if _, _, err = conn.Read(ctx); err != nil {
			return
		}
		if !send(map[string]any{"type": "session.updated"}) {
			return
		}
		for range 2 {
			if _, _, err = conn.Read(ctx); err != nil {
				return
			}
		}
		close(responseStarted)
		<-ctx.Done()
	}))
	defer server.Close()

	tts := New("dummy-token", "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/realtime", "dummy-model", "alloy", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := tts.Speak(ctx, "hello")
		done <- err
	}()
	select {
	case <-responseStarted:
	case <-time.After(time.Second):
		t.Fatal("request did not reach the waiting response")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted response after its deadline")
		}
	case <-time.After(time.Second):
		t.Fatal("Speak did not honor the response deadline")
	}
}

func TestPrebakePreservesExistingWAV(t *testing.T) {
	server := httptest.NewServer(dummyopenai.New("dummy").Handler())
	defer server.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "voice_down.wav")
	if err := os.WriteFile(path, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	tts := New("dummy", "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/realtime", "dummy", "alloy", dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	tts.Prebake(context.Background())
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "existing" {
		t.Fatalf("existing cache changed: %q %v", got, err)
	}
	for key := range Canned {
		if key == "voice_down" {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, key+".wav"))
		if readErr != nil || len(data) < 46 || string(data[:4]) != "RIFF" {
			t.Fatalf("%s: invalid WAV: %v", key, readErr)
		}
	}
}
