// Package dummyopenai implements a deterministic, non-AI subset of the
// OpenAI HTTP and Realtime sideband contracts used by Thornhill. It is a
// disposable conformance provider for tests; it never executes model output.
package dummyopenai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type Provider struct {
	Token string
	mu    sync.Mutex
	calls map[string]struct{}
}

func New(token string) *Provider {
	return &Provider{Token: token, calls: make(map[string]struct{})}
}

func (p *Provider) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", p.health)
	mux.HandleFunc("POST /v1/realtime/calls", p.createCall)
	mux.HandleFunc("GET /v1/realtime", p.sideband)
	mux.HandleFunc("POST /v1/audio/speech", p.speech)
	mux.HandleFunc("POST /v1/chat/completions", p.chat)
	return p.auth(mux)
}

func (p *Provider) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" && p.Token != "" && r.Header.Get("Authorization") != "Bearer "+p.Token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (p *Provider) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"ok","provider":"dummy-openai"}`)
}

func randomID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

func (p *Provider) createCall(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, "invalid multipart body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(r.FormValue("sdp")) == "" {
		http.Error(w, "missing sdp", http.StatusBadRequest)
		return
	}
	var session map[string]any
	if err := json.Unmarshal([]byte(r.FormValue("session")), &session); err != nil {
		http.Error(w, "invalid session json", http.StatusBadRequest)
		return
	}
	callID, err := randomID("rtc_dummy_")
	if err != nil {
		http.Error(w, "random id", http.StatusInternalServerError)
		return
	}
	p.mu.Lock()
	p.calls[callID] = struct{}{}
	p.mu.Unlock()
	w.Header().Set("Location", "/v1/realtime/calls/"+callID)
	w.Header().Set("Content-Type", "application/sdp")
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, "v=0\r\ns=dummy-openai\r\n")
}

func (p *Provider) sideband(w http.ResponseWriter, r *http.Request) {
	callID := r.URL.Query().Get("call_id")
	p.mu.Lock()
	_, ok := p.calls[callID]
	p.mu.Unlock()
	if !ok {
		http.Error(w, "unknown call_id", http.StatusNotFound)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")
	ctx := r.Context()
	if err := writeJSON(ctx, conn, map[string]any{"type": "session.created", "session": map[string]any{"id": callID}}); err != nil {
		return
	}
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var head struct {
			Type     string `json:"type"`
			EventID  string `json:"event_id"`
			Response struct {
				Metadata map[string]string `json:"metadata"`
			} `json:"response"`
		}
		if json.Unmarshal(raw, &head) != nil {
			_ = writeJSON(ctx, conn, map[string]any{"type": "error", "error": map[string]string{"type": "invalid_request_error", "code": "invalid_json", "message": "invalid JSON"}})
			continue
		}
		switch head.Type {
		case "session.update":
			_ = writeJSON(ctx, conn, map[string]any{"type": "session.updated", "session": map[string]any{"id": callID}})
		case "response.create":
			if head.EventID == "" {
				_ = writeJSON(ctx, conn, map[string]any{
					"type": "error",
					"error": map[string]string{
						"type":    "invalid_request_error",
						"code":    "missing_event_id",
						"message": "response.create requires event_id",
					},
				})
				continue
			}
			responseID, idErr := randomID("resp_dummy_")
			if idErr != nil {
				return
			}
			response := map[string]any{"id": responseID, "metadata": head.Response.Metadata}
			if writeJSON(ctx, conn, map[string]any{"type": "response.created", "response": response}) != nil {
				return
			}
			if writeJSON(ctx, conn, map[string]any{"type": "output_audio_buffer.started", "response_id": responseID}) != nil {
				return
			}
			if writeJSON(ctx, conn, map[string]any{"type": "response.output_audio_transcript.done", "transcript": "deterministic dummy response"}) != nil {
				return
			}
			response["status"] = "completed"
			response["usage"] = map[string]int{"input_tokens": 1, "output_tokens": 1}
			if writeJSON(ctx, conn, map[string]any{"type": "response.done", "response": response}) != nil {
				return
			}
			_ = writeJSON(ctx, conn, map[string]any{"type": "output_audio_buffer.stopped", "response_id": responseID})
		case "response.cancel":
			_ = writeJSON(ctx, conn, map[string]any{"type": "response.done", "response": map[string]any{"status": "cancelled"}})
		case "conversation.item.create":
			// Accepted deliberately. A response is emitted only after response.create.
		default:
			_ = writeJSON(ctx, conn, map[string]any{"type": "error", "error": map[string]string{"type": "invalid_request_error", "code": "unsupported_event", "message": "unsupported event type"}})
		}
	}
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, data)
}

func (p *Provider) speech(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Input string `json:"input"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request); err != nil || request.Input == "" {
		http.Error(w, "invalid speech request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	// Minimal deterministic marker, not synthesized audio.
	_, _ = w.Write([]byte("ID3\x04\x00\x00\x00\x00\x00\x00DUMMY"))
}

func (p *Provider) chat(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request); err != nil || len(request.Messages) == 0 {
		http.Error(w, "invalid chat request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":      "chatcmpl_dummy",
		"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "deterministic dummy summary"}}},
		"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1},
	})
}

func ParseEndpoint(line string) (baseURL, realtimeWSURL string, err error) {
	var endpoint struct {
		BaseURL       string `json:"base_url"`
		RealtimeWSURL string `json:"realtime_ws_url"`
	}
	if json.Unmarshal([]byte(line), &endpoint) != nil || endpoint.BaseURL == "" || endpoint.RealtimeWSURL == "" {
		return "", "", errors.New("invalid dummy provider endpoint line")
	}
	return endpoint.BaseURL, endpoint.RealtimeWSURL, nil
}

func EndpointJSON(baseURL string) ([]byte, error) {
	wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + "/v1/realtime"
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return nil, fmt.Errorf("unsupported base URL %q", baseURL)
	}
	return json.Marshal(map[string]string{"base_url": baseURL, "realtime_ws_url": wsURL})
}
