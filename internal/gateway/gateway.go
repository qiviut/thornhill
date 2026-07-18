// Package gateway is the HTTP face: the SDP signaling relay (the single
// API key never leaves the server), the browser control WebSocket, an SSE
// mirror of the bus, Hermes hook ingestion, and static assets.
package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"thornhill/internal/buildinfo"
	"thornhill/internal/config"
	"thornhill/internal/desk"
	"thornhill/internal/events"
	"thornhill/internal/notify"
	"thornhill/internal/store"
)

// Store is the slice of the persistence layer the gateway (and the desks
// it spawns) need. *store.Store satisfies it; tests use a fake.
type Store interface {
	desk.Summaries
	UsageTodayUSD(ctx context.Context) (float64, error)
}

type pushStore interface {
	UpsertPushSubscription(ctx context.Context, sub store.PushSubscription) error
	DeletePushSubscription(ctx context.Context, endpoint string) error
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
	deskWG   sync.WaitGroup
	closing  bool

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

// originAllowed admits non-browser clients (no Origin header), exact same-origin
// browser requests, and explicitly configured development origins. The normal
// production UI is same-origin, so its secure default is no extra configuration.
func (g *Gateway) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	candidateOrigin, candidateHost, ok := canonicalBrowserOrigin(origin)
	if !ok {
		return false
	}
	requestOrigin, ok := canonicalRequestOrigin(r)
	if ok && candidateOrigin == requestOrigin {
		return true
	}
	for _, pattern := range g.Cfg.AllowedOrigins {
		pattern = strings.ToLower(pattern)
		candidate := candidateHost
		if strings.Contains(pattern, "://") {
			candidate = candidateOrigin
		}
		if matched, err := path.Match(pattern, candidate); err == nil && matched {
			return true
		}
	}
	return false
}

func browserSameOrigin(r *http.Request) bool {
	candidateOrigin, _, ok := canonicalBrowserOrigin(r.Header.Get("Origin"))
	if !ok {
		return false
	}
	requestOrigin, ok := canonicalRequestOrigin(r)
	return ok && candidateOrigin == requestOrigin
}

func canonicalBrowserOrigin(origin string) (string, string, bool) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", false
	}
	canonical, ok := canonicalHTTPOrigin(u.Scheme, u.Host)
	return canonical, strings.ToLower(u.Host), ok
}

func canonicalRequestOrigin(r *http.Request) (string, bool) {
	scheme := strings.ToLower(r.URL.Scheme)
	if r.TLS != nil {
		scheme = "https"
	} else if forwarded := trustedForwardedProto(r); forwarded != "" {
		scheme = forwarded
	} else if scheme == "" {
		scheme = "http"
	}
	return canonicalHTTPOrigin(scheme, r.Host)
}

// trustedForwardedProto honors proxy metadata only on the documented local
// Tailscale Serve hop. A direct remote peer cannot redefine its request origin.
func trustedForwardedProto(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return ""
	}
	forwarded := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]))
	if forwarded != "http" && forwarded != "https" {
		return ""
	}
	return forwarded
}

func canonicalHTTPOrigin(scheme, hostport string) (string, bool) {
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	u, err := url.Parse(scheme + "://" + hostport)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return "", false
	}
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host, true
}

func (g *Gateway) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", g.handleStatus)
	mux.HandleFunc("GET /api/push/config", g.handlePushConfig)
	mux.HandleFunc("POST /api/push/subscriptions", g.handlePushSubscription)
	mux.HandleFunc("DELETE /api/push/subscriptions", g.handlePushSubscription)
	mux.HandleFunc("POST /offer", g.handleOffer)
	mux.HandleFunc("GET /ws", g.handleWS)
	mux.HandleFunc("GET /events", g.handleSSE)
	mux.HandleFunc("POST /hooks/hermes", g.Hooks)
	mux.Handle("GET /audio/prebaked/", http.StripPrefix("/audio/prebaked/",
		http.FileServer(http.Dir(g.Cfg.PrebakeDir))))
	mux.Handle("GET /", staticHandler(g.Cfg.StaticDir))
	return withLogging(mux, g.Log)
}

const immutableAssetCacheControl = "public, max-age=31536000, immutable"

// staticHandler compresses only Vite's text assets. Audio may already be
// compressed and can need byte ranges, so it stays on its dedicated identity
// handler above. Cache policy is injected only after FileServer chooses a
// successful response, so a transient missing asset cannot be held for a year.
func staticHandler(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/assets/") {
			// The HTML document names content-hashed assets. Revalidate it so a
			// deploy points browsers at the next asset generation promptly.
			if r.URL.Path == "/" || r.URL.Path == "/index.html" {
				w.Header().Set("Cache-Control", "no-cache")
			}
			files.ServeHTTP(w, r)
			return
		}

		cw := &cachedAssetWriter{ResponseWriter: w, gzip: acceptsGzipAsset(r)}
		files.ServeHTTP(cw, r)
		_ = cw.Close()
	})
}

func acceptsGzipAsset(r *http.Request) bool {
	if r.Method != http.MethodGet || r.Header.Get("Range") != "" {
		return false
	}
	switch strings.ToLower(path.Ext(r.URL.Path)) {
	case ".css", ".js", ".json", ".svg":
	default:
		return false
	}
	for _, token := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(token), ";")
		if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
			continue
		}
		for _, param := range strings.Split(params, ";") {
			key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") {
				continue
			}
			quality, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			return err == nil && quality > 0
		}
		return true
	}
	return false
}

type cachedAssetWriter struct {
	http.ResponseWriter
	gzip        bool
	gzipWriter  *gzip.Writer
	wroteHeader bool
}

func (w *cachedAssetWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	// Cache only a representation FileServer accepted. Vary keeps compressed
	// and identity responses distinct in shared and browser caches.
	if (status >= http.StatusOK && status < http.StatusMultipleChoices) || status == http.StatusNotModified {
		w.Header().Set("Cache-Control", immutableAssetCacheControl)
		w.Header().Add("Vary", "Accept-Encoding")
	}
	if w.gzip && status >= http.StatusOK && status < http.StatusMultipleChoices {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")
	} else {
		w.gzip = false
	}
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *cachedAssetWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if !w.gzip {
		return w.ResponseWriter.Write(p)
	}
	if w.gzipWriter == nil {
		w.gzipWriter = gzip.NewWriter(w.ResponseWriter)
	}
	return w.gzipWriter.Write(p)
}

func (w *cachedAssetWriter) Close() error {
	if !w.gzip {
		return nil
	}
	if w.gzipWriter == nil {
		w.gzipWriter = gzip.NewWriter(w.ResponseWriter)
	}
	return w.gzipWriter.Close()
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

type pushSubscriptionRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256DH string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

func (g *Gateway) handlePushConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	enabled := g.Cfg != nil && g.Cfg.PushVAPIDPublicKey != "" && g.Cfg.PushVAPIDPrivateKey != ""
	publicKey := ""
	if enabled {
		publicKey = g.Cfg.PushVAPIDPublicKey
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"enabled": enabled, "public_key": publicKey}); err != nil {
		g.Log.Error("write push config", "err", err)
	}
}

func (g *Gateway) handlePushSubscription(w http.ResponseWriter, r *http.Request) {
	if !browserSameOrigin(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	st, ok := g.Store.(pushStore)
	if !ok {
		http.Error(w, "push storage unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodDelete && (g.Cfg == nil || g.Cfg.PushVAPIDPublicKey == "" || g.Cfg.PushVAPIDPrivateKey == "") {
		http.Error(w, "push notifications are disabled", http.StatusServiceUnavailable)
		return
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input pushSubscriptionRequest
	if err := decoder.Decode(&input); err != nil {
		http.Error(w, "invalid push subscription", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid push subscription", http.StatusBadRequest)
		return
	}
	if err := validatePushEndpoint(input.Endpoint); err != nil {
		http.Error(w, "invalid push subscription endpoint", http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodDelete {
		if err := st.DeletePushSubscription(r.Context(), input.Endpoint); err != nil {
			g.Log.Warn("push subscription delete failed", "err", err)
			http.Error(w, "subscription storage failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := validatePushKeys(input.Keys.P256DH, input.Keys.Auth); err != nil {
		http.Error(w, "invalid push subscription keys", http.StatusBadRequest)
		return
	}
	if err := st.UpsertPushSubscription(r.Context(), store.PushSubscription{
		Endpoint: input.Endpoint, P256DH: input.Keys.P256DH, Auth: input.Keys.Auth,
	}); err != nil {
		g.Log.Warn("push subscription store failed", "err", err)
		http.Error(w, "subscription storage failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validatePushEndpoint(raw string) error {
	if len(raw) == 0 || len(raw) > 4096 {
		return errors.New("endpoint length")
	}
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return errors.New("endpoint must be an absolute HTTPS URL without userinfo or fragment")
	}
	host := strings.TrimSuffix(strings.ToLower(endpoint.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return errors.New("endpoint host must be public")
	}
	if ip := net.ParseIP(host); ip != nil && !notify.IsPublicPushIP(ip) {
		return errors.New("endpoint address must be public")
	}
	return nil
}

func validatePushKeys(p256dh, auth string) error {
	public, err := base64.RawURLEncoding.DecodeString(p256dh)
	if err != nil {
		return err
	}
	if _, err := ecdh.P256().NewPublicKey(public); err != nil {
		return err
	}
	authSecret, err := base64.RawURLEncoding.DecodeString(auth)
	if err != nil {
		return err
	}
	if len(authSecret) != 16 {
		return errors.New("auth secret must be 16 bytes")
	}
	return nil
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
	if g.closing {
		g.mu.Unlock()
		return
	}
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
	g.deskWG.Add(1)
	g.mu.Unlock()

	go func() {
		defer g.deskWG.Done()
		defer cancel()
		reason, err := d.Run(dctx, callID)
		switch {
		case err == nil:
			g.Log.Info("desk parked", "reason", reason)
		case errors.Is(err, context.Canceled):
			g.Log.Debug("desk cancelled", "reason", reason)
		default:
			g.Log.Warn("desk ended with error", "reason", reason, "err", err)
			g.voiceDown("session error: " + err.Error())
		}
		g.mu.Lock()
		if g.current == d {
			g.current = nil
			g.deskStop = nil
		}
		g.mu.Unlock()
	}()
}

// Shutdown prevents new desks, cancels the active generation, and waits for
// every superseded Desk goroutine before shared River/store dependencies close.
func (g *Gateway) Shutdown(ctx context.Context) error {
	g.mu.Lock()
	g.closing = true
	stop := g.deskStop
	g.mu.Unlock()
	if stop != nil {
		stop()
	}
	done := make(chan struct{})
	go func() {
		g.deskWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *Gateway) publishDeskState(d *desk.Desk, state desk.State, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	// Keep the current-generation check and publication in the same critical
	// section. Otherwise a replacement Desk can publish LIVE after the check
	// and then be torn down by the old Desk's stale PARKED event.
	if g.current == d {
		g.Bus.PublishSync(events.KindSessionState, "", map[string]string{"state": string(state), "reason": reason})
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
