// Package summarize maintains the "since you left" debrief as a rolling
// digest: every job event folds in via a cheap text model, debounced, so
// session resume is a single Postgres read with zero cold-start.
package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"thornhill/internal/events"
	"thornhill/internal/store"
)

type SummaryStore interface {
	GetSummary(ctx context.Context, scope string) (string, time.Time, error)
	SaveSummary(ctx context.Context, scope, content string) error
	AddUsage(ctx context.Context, source string, in, out int64, estUSD float64) error
}

type Summarizer struct {
	APIKey  string
	BaseURL string
	Model   string
	Store   SummaryStore
	Bus     *events.Bus
	Log     *slog.Logger
	HTTP    *http.Client
}

func New(apiKey, baseURL, model string, st SummaryStore, bus *events.Bus, log *slog.Logger) *Summarizer {
	return &Summarizer{APIKey: apiKey, BaseURL: strings.TrimRight(baseURL, "/"), Model: model, Store: st, Bus: bus, Log: log,
		HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// Run consumes the bus and folds noteworthy events into the debrief.
// Debounced: bursts collapse into one model call.
func (s *Summarizer) Run(ctx context.Context) {
	ch := s.Bus.Subscribe(ctx, false)
	var pending []string
	var timer *time.Timer
	fire := make(chan struct{}, 1)

	arm := func() {
		if timer != nil {
			timer.Stop()
		}
		timer = time.AfterFunc(20*time.Second, func() {
			select {
			case fire <- struct{}{}:
			default:
			}
		})
	}

	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			if line, keep := lineFor(e); keep {
				pending = append(pending, line)
				arm()
			}
		case <-fire:
			if len(pending) == 0 {
				continue
			}
			batch := pending
			pending = nil
			if err := s.fold(ctx, batch); err != nil {
				s.Log.Warn("debrief fold failed", "err", err)
			}
		}
	}
}

func lineFor(e events.Event) (string, bool) {
	var p struct {
		Name      string           `json:"display_name"`
		Digest    string           `json:"result_digest"`
		Error     string           `json:"error"`
		Question  string           `json:"question"`
		Approvals []store.Approval `json:"approvals"`
	}
	_ = json.Unmarshal(e.Payload, &p)
	ts := e.TS.Format("15:04")
	switch e.Kind {
	case events.KindJobDone:
		return fmt.Sprintf("%s job %q finished: %s", ts, p.Name, p.Digest), true
	case events.KindJobFailed:
		return fmt.Sprintf("%s job %q failed: %s", ts, p.Name, p.Error), true
	case events.KindJobNeedsInput:
		return fmt.Sprintf("%s job %q is waiting on a question: %s", ts, p.Name, p.Question), true
	case events.KindJobNeedsApproval:
		if len(p.Approvals) > 0 {
			return fmt.Sprintf("%s job %q needs approval: %s", ts, p.Name, p.Approvals[0].Description), true
		}
		return fmt.Sprintf("%s job %q needs approval", ts, p.Name), true
	case events.KindJobApprovalParked:
		return fmt.Sprintf("%s job %q parked an unresolved approval and released its run; resume requires fresh authority", ts, p.Name), true
	case events.KindJobQueued:
		return fmt.Sprintf("%s job %q dispatched", ts, p.Name), true
	case events.KindHermesHook:
		return "", false // heartbeats are noise for the debrief
	default:
		return "", false
	}
}

func (s *Summarizer) fold(ctx context.Context, lines []string) error {
	prev, _, err := s.Store.GetSummary(ctx, "debrief")
	if err != nil {
		return err
	}
	prompt := fmt.Sprintf(`You maintain a terse "since you left" digest for a voice assistant.
Fold the new events into the existing digest. Keep it under 120 words,
newest first, grouped by job, no fluff, no markdown. If the digest is
empty, start one.

EXISTING DIGEST:
%s

NEW EVENTS:
%s`, orNone(prev), strings.Join(lines, "\n"))

	out, err := s.complete(ctx, prompt)
	if err != nil {
		return err
	}
	return s.Store.SaveSummary(ctx, "debrief", strings.TrimSpace(out))
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(empty)"
	}
	return s
}

func (s *Summarizer) complete(ctx context.Context, prompt string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":    s.Model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("summary http %d: %s", resp.StatusCode, string(b))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", err
	}
	_ = s.Store.AddUsage(ctx, "summary", parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, 0)
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("no choices in summary response")
	}
	return parsed.Choices[0].Message.Content, nil
}
