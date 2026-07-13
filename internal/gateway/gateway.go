// Package gateway is the HTTP face: the SDP signaling relay (the single
// API key never leaves the server), the browser control WebSocket, an SSE
// mirror of the bus, Hermes hook ingestion, and static assets.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"thornhill/internal/buildinfo"
	"thornhill/internal/config"
	"thornhill/internal/desk"
	"thornhill/internal/events"
)

// Store is the slice of the persistence layer the gateway (and the desks
// it spawns) need. *store.Store satisfies it; tests use a fake.
type Store interface {
	desk.Summaries
	UsageTodayUSD(ctx context.Context) (float64, error)
}

type Gateway struct {
	Cfg        *config.Config
	Bus        *events.Bus
	Store      Store
	Dispatcher desk.Dispatcher
	Hooks      http.HandlerFunc
	Log        *slog.Logger
	Upstream   string // override in tests; default https://api.openai.com

	mu       sync.Mutex
	deskStop context.CancelFunc
	current  *desk.Desk

	// deskLaunch overrides desk startup in tests; nil means g.startDesk.
	deskLaunch func(callID string)
}

func (g *Gateway) upstream() string {
	if g.Upstream != "" {
		return g.Upstream
	}
	if g.Cfg != nil && g.Cfg.OpenAIBaseURL != "" {
		return strings.TrimRight(g.Cfg.OpenAIBaseURL, "/")
	}
	return "https://api.openai.com"
}

// originAllowed admits non-browser clients (no Origin header), same-origin
// browser requests, and explicitly configured development origins. The normal
// production UI is same-origin, so its secure default is no extra configuration.
func (g *Gateway) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	for _, pattern := range g.Cfg.AllowedOrigins {
		pattern = strings.ToLower(pattern)
		candidate := strings.ToLower(u.Host)
		if strings.Contains(pattern, "://") {
			candidate = strings.ToLower(origin)
		}
		if matched, err := path.Match(pattern, candidate); err == nil && matched {
			return true
		}
	}
	return false
}

func (g *Gateway) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", g.handleStatus)
	mux.HandleFunc("POST /offer", g.handleOffer)
	mux.HandleFunc("GET /ws", g.handleWS)
	mux.HandleFunc("GET /events", g.handleSSE)
	mux.HandleFunc("POST /hooks/hermes", g.Hooks)
	mux.Handle("GET /audio/prebaked/", http.StripPrefix("/audio/prebaked/",
		http.FileServer(http.Dir(g.Cfg.PrebakeDir))))
	mux.Handle("GET /", http.FileServer(http.Dir(g.Cfg.StaticDir)))
	return withLogging(mux, g.Log)
}

func (g *Gateway) handleStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	status := "ok"
	if !buildinfo.Valid() {
		status = "unversioned"
	}
	if err := json.NewEncoder(w).Encode(map[string]any{
		"status":        status,
		"source_commit": buildinfo.Commit,
		"versioned":     buildinfo.Valid(),
	}); err != nil {
		g.Log.Error("write status", "err", err)
	}
}

func withLogging(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/events" { // SSE would log once per connection lifetime anyway
			log.Debug("http", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start))
		}
	})
}

// handleOffer relays the browser's SDP to POST {upstream}/v1/realtime/calls
// as multipart form data (fields per docs/vendor/openai/realtime-webrtc.md:
// "sdp" and "session"), captures the call_id from the Location header, and
// spins up a Desk on the sideband. Budget breaker sits here: no new calls
// past the daily estimate.
func (g *Gateway) handleOffer(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if !g.originAllowed(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	offer, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(bytes.TrimSpace(offer)) == 0 {
		http.Error(w, "empty sdp offer", http.StatusBadRequest)
		return
	}

	if usd, err := g.Store.UsageTodayUSD(r.Context()); err == nil && usd >= g.Cfg.DailyBudgetUSD {
		g.Bus.Publish(events.KindErrorVoice, "", map[string]string{"message": "daily budget reached", "play": "budget_tripped"})
		http.Error(w, "daily budget reached", http.StatusTooManyRequests)
		return
	}

	sessionCfg, _ := json.Marshal(map[string]any{
		"type":  "realtime",
		"model": g.Cfg.RealtimeModel,
		"audio": map[string]any{
			"input": map[string]any{
				// Establish Desk-owned response serialization in the atomic call
				// configuration. Waiting for the later sideband session.update
				// leaves a first-utterance race with the provider defaults.
				"turn_detection": map[string]any{
					"type":               "semantic_vad",
					"create_response":    false,
					"interrupt_response": false,
				},
			},
			"output": map[string]any{"voice": g.Cfg.Voice},
		},
	})

	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	_ = mw.WriteField("sdp", string(offer))
	_ = mw.WriteField("session", string(sessionCfg))
	_ = mw.Close()

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		g.upstream()+"/v1/realtime/calls", &form)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+g.Cfg.OpenAIKey)
	req.Header.Set("OpenAI-Safety-Identifier", g.Cfg.SafetyID)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		g.voiceDown("call create failed: " + err.Error())
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		g.voiceDown(fmt.Sprintf("call create http %d: %s", resp.StatusCode, truncate(string(answer), 200)))
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	loc := resp.Header.Get("Location") // e.g. /v1/realtime/calls/rtc_123
	parts := strings.Split(strings.TrimRight(loc, "/"), "/")
	callID := parts[len(parts)-1]
	if !validCallID(callID) {
		g.Log.Warn("invalid call id from Location", "location", loc)
		http.Error(w, "upstream response missing a valid call id", http.StatusBadGateway)
		return
	}
	g.Log.Info("call created", "call_id", callID)

	launch := g.deskLaunch
	if launch == nil {
		launch = g.startDesk
	}
	launch(callID)

	w.Header().Set("Content-Type", "application/sdp")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(answer)
}

func validCallID(callID string) bool {
	if !strings.HasPrefix(callID, "rtc_") || len(callID) == len("rtc_") {
		return false
	}
	for _, r := range callID[len("rtc_"):] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func (g *Gateway) startDesk(callID string) {
	g.mu.Lock()
	if g.deskStop != nil {
		g.deskStop() // single user: a new call supersedes the old desk
	}
	dctx, cancel := context.WithCancel(context.Background())
	g.deskStop = cancel
	var d *desk.Desk
	d = desk.New(desk.Deps{
		APIKey:          g.Cfg.OpenAIKey,
		RealtimeWSURL:   g.Cfg.OpenAIRealtimeWSURL,
		TranscribeModel: g.Cfg.TranscribeModel,
		Dispatcher:      g.Dispatcher,
		Store:           g.Store,
		Bus:             g.Bus,
		Log:             g.Log.With("comp", "desk"),
		FSMConfig:       desk.DefaultFSMConfig(g.Cfg.QuietAfter, g.Cfg.ParkAfter, g.Cfg.RolloverAt),
		PublishState: func(state desk.State, reason string) {
			g.publishDeskState(d, state, reason)
		},
	})
	g.current = d
	g.mu.Unlock()

	go func() {
		defer cancel()
		reason, err := d.Run(dctx, callID)
		if err != nil {
			g.Log.Warn("desk ended with error", "reason", reason, "err", err)
			g.voiceDown("session error: " + err.Error())
		} else {
			g.Log.Info("desk parked", "reason", reason)
		}
		g.mu.Lock()
		if g.current == d {
			g.current = nil
			g.deskStop = nil
		}
		g.mu.Unlock()
	}()
}

func (g *Gateway) publishDeskState(d *desk.Desk, state desk.State, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	// Keep the current-generation check and publication in the same critical
	// section. Otherwise a replacement Desk can publish LIVE after the check
	// and then be torn down by the old Desk's stale PARKED event.
	if g.current == d {
		g.Bus.Publish(events.KindSessionState, "", map[string]string{"state": string(state), "reason": reason})
	}
}

func (g *Gateway) voiceDown(msg string) {
	g.Log.Warn("voice down", "msg", msg)
	g.Bus.Publish(events.KindErrorVoice, "", map[string]string{"message": msg, "play": "voice_down"})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// --- control WebSocket ---

type clientMsg struct {
	Type   string `json:"type"` // state|text|park
	Muted  bool   `json:"muted"`
	Hidden bool   `json:"hidden"`
	Text   string `json:"text"`
}

func (g *Gateway) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The request host is accepted by default. ALLOWED_ORIGINS can add
		// an explicit Vite development origin without opening CSWSH broadly.
		OriginPatterns: g.Cfg.AllowedOrigins,
	})
	if err != nil {
		g.Log.Warn("ws accept failed", "err", err)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer conn.Close(websocket.StatusNormalClosure, "bye")
	g.Log.Info("control ws connected", "remote", r.RemoteAddr)

	// Writer: bus events (with replay) + heartbeat.
	go func() {
		evs := g.Bus.Subscribe(ctx, true)
		hb := time.NewTicker(5 * time.Second)
		defer hb.Stop()
		write := func(v any) bool {
			data, err := json.Marshal(v)
			if err != nil {
				return true
			}
			wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
			werr := conn.Write(wctx, websocket.MessageText, data)
			wcancel()
			return werr == nil
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-hb.C:
				if !write(map[string]string{"type": "hb"}) {
					cancel()
					return
				}
			case e, ok := <-evs:
				if !ok {
					return
				}
				if !write(map[string]any{"type": "event", "event": e}) {
					cancel()
					return
				}
			}
		}
	}()

	// Reader: client state and text fallback.
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			g.Log.Debug("control ws closed", "err", err)
			return
		}
		var m clientMsg
		if err := json.Unmarshal(data, &m); err != nil {
			g.Log.Debug("bad ws message", "err", err)
			continue
		}
		g.mu.Lock()
		d := g.current
		g.mu.Unlock()
		switch m.Type {
		case "state":
			if d != nil {
				d.SetClientState(m.Muted, m.Hidden)
			}
		case "text":
			if d != nil {
				d.InjectUserText(m.Text)
			} else {
				g.Bus.Publish(events.KindErrorVoice, "",
					map[string]string{"message": "no live session; text queued nowhere — resume first"})
			}
		case "park":
			if d != nil {
				d.RequestPark()
			}
		}
	}
}

// --- SSE mirror (debugging, curl-ability) ---

func (g *Gateway) handleSSE(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	evs := g.Bus.Subscribe(r.Context(), true)
	for e := range evs {
		data, err := json.Marshal(e)
		if err != nil {
			continue
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
		fl.Flush()
	}
}
