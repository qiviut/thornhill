// Thornhill: single-operator voice desk for a Hermes Agent fleet.
// One OpenAI key, tailnet-only listener, Postgres, crash-only sessions.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"thornhill/internal/audio"
	"thornhill/internal/bridge"
	"thornhill/internal/buildinfo"
	"thornhill/internal/config"
	"thornhill/internal/dispatch"
	"thornhill/internal/events"
	"thornhill/internal/gateway"
	"thornhill/internal/store"
	"thornhill/internal/summarize"
)

const defaultHealthcheckURL = "http://127.0.0.1:8787/api/status"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Printf("thornhill %s\n", buildinfo.Commit)
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if err := runHealthcheck(os.Getenv("HEALTHCHECK_URL")); err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck:", err)
			os.Exit(1)
		}
		return
	}
	lvl := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		lvl = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(log)
	if !buildinfo.Valid() {
		log.Warn("unversioned build; CI-correspondence checks will reject deployment", "source_commit", buildinfo.Commit)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL, log.With("comp", "store"))
	if err != nil {
		log.Error("store", "err", err)
		os.Exit(1)
	}
	defer st.Pool.Close()

	bus := events.NewBus(st, log.With("comp", "bus"))

	var runner dispatch.Runner
	if cfg.HermesBaseURL == "" {
		log.Warn("HERMES_BASE_URL empty — stub runner active", "fake_seconds", cfg.FakeJobSeconds)
		runner = dispatch.NewStubRunner(st, bus,
			time.Duration(cfg.FakeJobSeconds)*time.Second, log.With("comp", "stub"))
	} else {
		log.Info("hermes bridge active", "base", cfg.HermesBaseURL, "model", cfg.HermesModel)
		hermes := bridge.NewHermes(cfg.HermesBaseURL, cfg.HermesAPIKey, cfg.HermesModel,
			st, bus, log.With("comp", "hermes"))
		hermes.ApprovalParkAfter = cfg.ApprovalParkAfter
		if err := hermes.ReconcileOrphans(ctx); err != nil {
			log.Error("reconcile orphaned Hermes runs", "err", err)
			os.Exit(1)
		}
		runner = hermes
	}

	queue, stopRiver, err := dispatch.StartRiver(ctx, st.Pool, runner, log.With("comp", "river"))
	if err != nil {
		log.Error("river", "err", err)
		os.Exit(1)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := stopRiver(sctx); err != nil {
			log.Warn("river stop", "err", err)
		}
	}()

	disp := dispatch.New(st, bus, queue, runner, log.With("comp", "dispatch"))

	summ := summarize.New(cfg.OpenAIKey, cfg.OpenAIBaseURL, cfg.SummaryModel, st, bus, log.With("comp", "summarize"))
	go summ.Run(ctx)

	tts := audio.New(cfg.OpenAIKey, cfg.OpenAIBaseURL, cfg.TTSModel, cfg.TTSVoice, cfg.PrebakeDir, log.With("comp", "tts"))
	go tts.Prebake(ctx)

	hooks := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if h, ok := runner.(*bridge.Hermes); ok {
		hooks = h.HooksHandler()
	}

	gw := &gateway.Gateway{
		Cfg: cfg, Bus: bus, Store: st, Dispatcher: disp,
		Hooks: hooks, Log: log.With("comp", "gateway"),
	}
	// Do not set WriteTimeout: the service deliberately holds SSE and WebSocket
	// responses open. Header and request-body deadlines still bound slowloris
	// connections and oversized SDP submissions.
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           gw.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		<-ctx.Done()
		log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	log.Info("thornhill up",
		"addr", cfg.ListenAddr,
		"source_commit", buildinfo.Commit,
		"realtime_model", cfg.RealtimeModel,
		"stub", cfg.HermesBaseURL == "")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
	log.Info("bye")
}

func runHealthcheck(rawURL string) error {
	if rawURL == "" {
		rawURL = defaultHealthcheckURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}
	var status struct {
		Status       string `json:"status"`
		SourceCommit string `json:"source_commit"`
		Versioned    bool   `json:"versioned"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return fmt.Errorf("decode status: %w", err)
	}
	if buildinfo.Valid() {
		if status.Status != "ok" || !status.Versioned || status.SourceCommit != buildinfo.Commit {
			return fmt.Errorf("unhealthy status=%q versioned=%t source_commit=%q", status.Status, status.Versioned, status.SourceCommit)
		}
		return nil
	}
	// Development images are intentionally allowed to run without fabricated
	// provenance. CI and the deployer reject them for promotion, while the
	// Docker health check still provides a useful liveness/readiness signal.
	if status.Status != "unversioned" || status.Versioned || status.SourceCommit != buildinfo.Commit {
		return fmt.Errorf("unhealthy unversioned status=%q versioned=%t source_commit=%q", status.Status, status.Versioned, status.SourceCommit)
	}
	return nil
}
