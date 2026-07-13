// Package openairt is a minimal client for the OpenAI Realtime API GA
// sideband channel: a WebSocket attached to an existing WebRTC call via
// call_id. Event names and payload shapes follow the vendored docs in
// docs/vendor/openai/ (realtime-conversations.md, realtime-server-controls.md,
// realtime-webrtc.md). Where a field is unconfirmed it carries TODO(verify).
package openairt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"
)

// Server event types we react to (subset; unknown types are logged at debug
// and ignored, which keeps us forward-compatible with API additions).
const (
	EvSessionCreated       = "session.created"
	EvSessionUpdated       = "session.updated"
	EvSpeechStarted        = "input_audio_buffer.speech_started"
	EvSpeechStopped        = "input_audio_buffer.speech_stopped"
	EvInputAudioCommitted  = "input_audio_buffer.committed"
	EvInputTranscriptDone  = "conversation.item.input_audio_transcription.completed"
	EvResponseCreated      = "response.created"
	EvOutputAudioStarted   = "output_audio_buffer.started"
	EvOutputAudioStopped   = "output_audio_buffer.stopped"
	EvOutputAudioCleared   = "output_audio_buffer.cleared"
	EvOutputTranscriptDone = "response.output_audio_transcript.done"
	EvFuncArgsDone         = "response.function_call_arguments.done"
	EvResponseDone         = "response.done"
	EvError                = "error"
)

// ServerEvent is the loosely-typed envelope; specific fields are pulled out
// lazily because the GA schema is wide and we only need a corner of it.
type ServerEvent struct {
	Type string `json:"type"`
	Raw  json.RawMessage
}

// FuncCall is a completed function call extracted from a response.
type FuncCall struct {
	CallID    string
	Name      string
	Arguments string // JSON string per the wire format
}

// Usage as carried on response.done.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type Tool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type Client struct {
	conn *websocket.Conn
	log  *slog.Logger
}

// Dial attaches to an in-progress call. Contract per
// realtime-server-controls.md: wss://api.openai.com/v1/realtime?call_id=...
// with a standard bearer key.
func Dial(ctx context.Context, apiKey, callID string, log *slog.Logger) (*Client, error) {
	return DialURL(ctx, apiKey, callID, "wss://api.openai.com/v1/realtime", log)
}

// DialURL attaches to a configurable Realtime-compatible sideband endpoint.
// Production uses OpenAI; tests may use a deterministic local provider.
func DialURL(ctx context.Context, apiKey, callID, baseURL string, log *slog.Logger) (*Client, error) {
	if baseURL == "" {
		baseURL = "wss://api.openai.com/v1/realtime"
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "wss" && u.Scheme != "ws") {
		return nil, errors.New("Realtime sideband endpoint must be an absolute ws or wss URL")
	}
	if u.User != nil || u.Fragment != "" {
		return nil, errors.New("Realtime sideband endpoint must not contain userinfo or a fragment")
	}
	if u.Scheme == "ws" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("Realtime sideband endpoint must use wss except on loopback")
		}
	}
	query := u.Query()
	query.Set("call_id", callID)
	u.RawQuery = query.Encode()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+apiKey)
	conn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		return nil, fmt.Errorf("sideband dial: %w", err)
	}
	conn.SetReadLimit(1 << 22) // transcripts and tool args, not audio; 4 MiB is generous
	log.Info("sideband attached", "call_id", callID)
	return &Client{conn: conn, log: log}, nil
}

func (c *Client) Close() {
	if c != nil && c.conn != nil {
		c.conn.Close(websocket.StatusNormalClosure, "bye")
	}
}

// Read blocks for the next server event.
func (c *Client) Read(ctx context.Context) (ServerEvent, error) {
	_, data, err := c.conn.Read(ctx)
	if err != nil {
		return ServerEvent{}, err
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return ServerEvent{}, fmt.Errorf("event decode: %w", err)
	}
	return ServerEvent{Type: head.Type, Raw: data}, nil
}

func (c *Client) send(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.log.Debug("sideband send", "payload", truncate(string(data), 300))
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.conn.Write(wctx, websocket.MessageText, data)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// SessionUpdate pushes instructions, tools, transcription and truncation
// policy onto the live session. Shape per realtime-webrtc.md session config
// plus realtime-costs.md truncation.
func (c *Client) SessionUpdate(ctx context.Context, instructions string, tools []Tool, transcribeModel string) error {
	type turnDetection struct {
		Type              string `json:"type"`
		CreateResponse    bool   `json:"create_response"`
		InterruptResponse bool   `json:"interrupt_response"`
	}
	payload := map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type":         "realtime",
			"instructions": instructions,
			"tools":        tools,
			"audio": map[string]any{
				"input": map[string]any{
					"transcription": map[string]any{"model": transcribeModel}, // TODO(verify) model name
					// Thornhill owns response creation. Microphone noise or echo
					// must not cancel audible output, and the desk must serialize
					// user turns, tool continuations, and announcements.
					"turn_detection": turnDetection{
						Type: "semantic_vad", CreateResponse: false, InterruptResponse: false,
					},
				},
			},
			// TODO(verify): exact truncation field path per realtime-costs.md
			"truncation": map[string]any{"type": "retention_ratio", "retention_ratio": 0.8},
		},
	}
	return c.send(ctx, payload)
}

// InjectMessage appends a conversation item without triggering a response.
// role: "user" or "system". Use CreateResponse afterwards at a turn boundary.
func (c *Client) InjectMessage(ctx context.Context, role, text string) error {
	return c.send(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message",
			"role": role,
			"content": []map[string]any{
				{"type": "input_text", "text": text},
			},
		},
	})
}

// FunctionOutput answers a pending function call.
func (c *Client) FunctionOutput(ctx context.Context, callID, output string) error {
	return c.send(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  output,
		},
	})
}

// CreateResponse asks the model to speak/act on the default conversation.
// eventID correlates any asynchronous server error with this exact request.
func (c *Client) CreateResponse(ctx context.Context, eventID string) error {
	return c.send(ctx, map[string]any{
		"event_id": eventID,
		"type":     "response.create",
	})
}

// CreateOOB runs an out-of-band response (conversation: "none") — used for
// side work like end-of-session summaries without touching conversation state.
func (c *Client) CreateOOB(ctx context.Context, instructions string, meta map[string]string) error {
	return c.send(ctx, map[string]any{
		"type": "response.create",
		"response": map[string]any{
			"conversation":      "none",
			"metadata":          meta,
			"output_modalities": []string{"text"},
			"instructions":      instructions,
		},
	})
}

func (c *Client) CancelResponse(ctx context.Context) error {
	return c.send(ctx, map[string]any{"type": "response.cancel"})
}

// --- server event field extraction ---

// ExtractFuncCalls pulls completed function calls out of a response.done
// event (items of type function_call in response.output), which per
// realtime-conversations.md carries call_id, name and arguments.
func ExtractFuncCalls(raw json.RawMessage) []FuncCall {
	var body struct {
		Response struct {
			Status string `json:"status"`
			Output []struct {
				Type      string `json:"type"`
				Status    string `json:"status"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Response.Status != "completed" {
		return nil
	}
	var out []FuncCall
	for _, it := range body.Response.Output {
		if it.Type == "function_call" && it.Status == "completed" && it.CallID != "" && it.Name != "" {
			out = append(out, FuncCall{CallID: it.CallID, Name: it.Name, Arguments: it.Arguments})
		}
	}
	return out
}

// ExtractUsage pulls token usage off response.done.
func ExtractUsage(raw json.RawMessage) Usage {
	var body struct {
		Response struct {
			Usage Usage `json:"usage"`
		} `json:"response"`
	}
	_ = json.Unmarshal(raw, &body)
	return body.Response.Usage
}

// ExtractTranscript pulls the transcript string off input/output transcript
// events; both carry a top-level "transcript" field in GA.
func ExtractTranscript(raw json.RawMessage) string {
	var body struct {
		Transcript string `json:"transcript"`
	}
	_ = json.Unmarshal(raw, &body)
	return body.Transcript
}

// ExtractError renders an error event human-readable.
func ExtractError(raw json.RawMessage) string {
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return string(raw)
	}
	return fmt.Sprintf("%s/%s: %s", body.Error.Type, body.Error.Code, body.Error.Message)
}

// ExtractErrorCode returns the stable machine-readable Realtime error code.
func ExtractErrorCode(raw json.RawMessage) string {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	return body.Error.Code
}

// ExtractErrorEventID returns the client event_id associated with an
// asynchronous Realtime error. GA error events place it inside "error";
// accepting a top-level value keeps correlation robust across documented
// examples and provider-compatible fixtures.
func ExtractErrorEventID(raw json.RawMessage) string {
	var body struct {
		EventID string `json:"event_id"`
		Error   struct {
			EventID string `json:"event_id"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	if body.Error.EventID != "" {
		return body.Error.EventID
	}
	return body.EventID
}

// ExtractResponseStatus distinguishes completed output from cancellation,
// failure, and incomplete generation. Malformed payloads return "".
func ExtractResponseStatus(raw json.RawMessage) string {
	var body struct {
		Response struct {
			Status string `json:"status"`
		} `json:"response"`
	}
	_ = json.Unmarshal(raw, &body)
	return body.Response.Status
}
