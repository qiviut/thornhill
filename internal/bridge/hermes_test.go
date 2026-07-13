package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"thornhill/internal/events"
	"thornhill/internal/store"
)

type fakeStore struct {
	mu        sync.Mutex
	jobs      map[string]store.Job
	permanent map[string]bool
	allows    map[string]bool
}

func (s *fakeStore) UpdateJob(_ context.Context, id string, mut func(*store.Job)) (store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return store.Job{}, store.ErrNotFound
	}
	mut(&j)
	j.UpdatedAt = time.Now().UTC()
	s.jobs[id] = j
	return j, nil
}

func (s *fakeStore) ActiveJobs(_ context.Context) ([]store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		if j.Status == store.StatusQueued || j.Status == store.StatusRunning || j.Status == store.StatusNeedsInput || j.Status == store.StatusNeedsApproval {
			out = append(out, j)
		}
	}
	return out, nil
}

func (s *fakeStore) ClaimApproval(_ context.Context, jobID, approvalID, nonce string) (store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return store.Job{}, store.ErrNotFound
	}
	if j.Status != store.StatusNeedsApproval || len(j.Approvals) != 1 ||
		j.Approvals[0].ID != approvalID || j.Approvals[0].DecisionNonce != nonce ||
		j.Approvals[0].State != "pending" {
		return j, store.ErrApprovalStale
	}
	j.Approvals[0].State = "sending"
	s.jobs[jobID] = j
	return j, nil
}

func (s *fakeStore) ResolveJob(_ context.Context, ref string) (store.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[ref]
	if !ok {
		return store.Job{}, store.ErrNotFound
	}
	return j, nil
}

func (s *fakeStore) SavePermanentDenials(_ context.Context, keys []string, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.permanent == nil {
		s.permanent = map[string]bool{}
	}
	s.permanent[store.ApprovalPatternHash(keys)] = true
	return nil
}

func (s *fakeStore) MatchesPermanentDenial(_ context.Context, keys []string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := store.ApprovalPatternHash(keys)
	if s.permanent[hash] {
		return hash, nil
	}
	return "", nil
}

func (s *fakeStore) SavePermanentAllows(_ context.Context, keys []string, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.allows == nil {
		s.allows = map[string]bool{}
	}
	s.allows[store.ApprovalPatternHash(keys)] = true
	return nil
}

func (s *fakeStore) MatchesPermanentAllow(_ context.Context, keys []string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := store.ApprovalPatternHash(keys)
	if s.allows[hash] {
		return hash, nil
	}
	return "", nil
}

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ownRun(h *Hermes, jobID, runID string) {
	h.mu.Lock()
	h.runIDs[jobID] = runID
	h.mu.Unlock()
}

func TestRunsAPIStreamsProgressAndResolvesApproval(t *testing.T) {
	const jobID = "01JTESTTHORNHILL"
	approvalChoice := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			var body struct {
				Input        string    `json:"input"`
				Instructions string    `json:"instructions"`
				SessionID    string    `json:"session_id"`
				History      []chatMsg `json:"conversation_history"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode start: %v", err)
			}
			if body.Input != "audit" || body.SessionID != jobID || !strings.Contains(body.Instructions, "Audit") {
				t.Errorf("unexpected start body: %+v", body)
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"run_id":"run_1","status":"started"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/run_1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			_, _ = io.WriteString(w, "data: {\"event\":\"tool.started\",\"tool\":\"terminal\",\"preview\":\"inspect services\"}\n\n")
			_, _ = io.WriteString(w, "data: {\"event\":\"approval.request\",\"command\":\"systemctl restart demo\",\"description\":\"restart demo service\",\"pattern_keys\":[\"service restart\"]}\n\n")
			flusher.Flush()
			if got := <-approvalChoice; got != "once" {
				t.Errorf("approval choice = %q", got)
			}
			_, _ = io.WriteString(w, "data: {\"event\":\"tool.completed\",\"tool\":\"terminal\"}\n\n")
			_, _ = io.WriteString(w, "data: {\"event\":\"run.completed\",\"output\":\"done\"}\n\n")
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/run_1/approval":
			var body struct {
				Choice string `json:"choice"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			approvalChoice <- body.Choice
			_, _ = io.WriteString(w, `{"resolved":1}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	fs := &fakeStore{jobs: map[string]store.Job{jobID: {
		ID: jobID, DisplayName: "Audit", Task: "audit", Status: store.StatusRunning,
	}}, permanent: map[string]bool{}}
	bus := events.NewBus(nil, testLog())
	h := NewHermes(server.URL, "test-key", "hermes-agent", fs, bus, testLog())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sub := bus.Subscribe(ctx, false)

	result := make(chan struct {
		out string
		err error
	}, 1)
	go func() {
		out, err := h.runTurn(ctx, fs.jobs[jobID], "audit", nil)
		result <- struct {
			out string
			err error
		}{out, err}
	}()

	for {
		select {
		case e := <-sub:
			if e.Kind != events.KindJobNeedsApproval {
				continue
			}
			pending, _ := fs.ResolveJob(ctx, jobID)
			if len(pending.Approvals) != 1 {
				t.Fatalf("pending approvals: %+v", pending.Approvals)
			}
			a := pending.Approvals[0]
			j, err := h.DecideApproval(ctx, jobID, a.ID, a.DecisionNonce, DecisionAllowOnce)
			if err != nil {
				t.Fatalf("decide approval: %v", err)
			}
			if j.Status != store.StatusRunning || len(j.Approvals) != 0 {
				t.Fatalf("post-decision job: %+v", j)
			}
			goto resolved
		case <-ctx.Done():
			t.Fatal("approval event timeout")
		}
	}

resolved:
	got := <-result
	if got.err != nil || got.out != "done" {
		t.Fatalf("runTurn = %q, %v", got.out, got.err)
	}
	j, _ := fs.ResolveJob(ctx, jobID)
	if j.HermesRunID != "run_1" || j.Progress == nil || j.Progress.State != "completed" {
		t.Fatalf("final progress state: %+v", j)
	}
}

func TestHealthyRunHasNoApprovalDecisionWindow(t *testing.T) {
	t.Parallel()
	const jobID = "job-long"
	var stopCalls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"run_id":"run-long","status":"started"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/run-long/events":
			time.Sleep(80 * time.Millisecond)
			_, _ = io.WriteString(w, "data: {\"event\":\"run.completed\",\"output\":\"long done\"}\n\n")
		case strings.HasSuffix(r.URL.Path, "/stop"):
			mu.Lock()
			stopCalls++
			mu.Unlock()
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	fs := &fakeStore{jobs: map[string]store.Job{jobID: {ID: jobID, DisplayName: "Long", Status: store.StatusRunning}}}
	h := NewHermes(srv.URL, "", "hermes-agent", fs, events.NewBus(nil, testLog()), testLog())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	out, err := h.runTurn(ctx, fs.jobs[jobID], "work", nil)
	if err != nil || out != "long done" {
		t.Fatalf("runTurn = %q, %v", out, err)
	}
	if elapsed := time.Since(started); elapsed < 75*time.Millisecond {
		t.Fatalf("run returned unexpectedly early after %s", elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	if stopCalls != 0 {
		t.Fatalf("healthy long run received %d stop calls", stopCalls)
	}
}

func TestApprovalWaitsIndefinitelyForExplicitDecision(t *testing.T) {
	t.Parallel()
	const jobID = "job-approval-wait"
	resolved := make(chan struct{})
	var approvalBody map[string]any
	var stopCalls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"run_id":"run-wait","status":"started"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/run-wait/events":
			flusher := w.(http.Flusher)
			_, _ = io.WriteString(w, "data: {\"event\":\"approval.request\",\"command\":\"risky\",\"description\":\"test\",\"pattern_keys\":[\"test:risky\"]}\n\n")
			flusher.Flush()
			select {
			case <-resolved:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, "data: {\"event\":\"run.completed\",\"output\":\"continued\"}\n\n")
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/run-wait/approval":
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&approvalBody)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"resolved":1}`)
			close(resolved)
		case strings.HasSuffix(r.URL.Path, "/stop"):
			mu.Lock()
			stopCalls++
			mu.Unlock()
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	fs := &fakeStore{jobs: map[string]store.Job{jobID: {ID: jobID, DisplayName: "Waiting", Status: store.StatusRunning}}}
	h := NewHermes(srv.URL, "", "hermes-agent", fs, events.NewBus(nil, testLog()), testLog())
	h.ApprovalControlTimeout = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type turnResult struct {
		out string
		err error
	}
	result := make(chan turnResult, 1)
	go func() {
		out, err := h.runTurn(ctx, fs.jobs[jobID], "work", nil)
		result <- turnResult{out: out, err: err}
	}()

	var j store.Job
	deadline := time.Now().Add(time.Second)
	for {
		j, _ = fs.ResolveJob(ctx, jobID)
		if j.Status == store.StatusNeedsApproval && len(j.Approvals) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never reached pending approval: %+v", j)
		}
		time.Sleep(time.Millisecond)
	}

	// There is intentionally no decision deadline. The exact approval remains
	// pending and no deny/stop control request is emitted in the background.
	time.Sleep(100 * time.Millisecond)
	j, _ = fs.ResolveJob(ctx, jobID)
	if j.Status != store.StatusNeedsApproval || len(j.Approvals) != 1 {
		t.Fatalf("approval did not remain pending: %+v", j)
	}
	mu.Lock()
	if approvalBody != nil || stopCalls != 0 {
		mu.Unlock()
		t.Fatalf("background decision=%#v stopCalls=%d", approvalBody, stopCalls)
	}
	mu.Unlock()

	approval := j.Approvals[0]
	if _, err := h.DecideApproval(ctx, jobID, approval.ID, approval.DecisionNonce, DecisionAllowOnce); err != nil {
		t.Fatalf("explicit approval: %v", err)
	}
	got := <-result
	if got.err != nil || got.out != "continued" {
		t.Fatalf("runTurn = %q, %v", got.out, got.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if approvalBody["choice"] != "once" || approvalBody["resolve_all"] != false {
		t.Fatalf("approval body = %#v", approvalBody)
	}
	if stopCalls != 0 {
		t.Fatalf("explicit approval stopped healthy run %d times", stopCalls)
	}
}

func TestPermanentDenyAutoResolvesExactPattern(t *testing.T) {
	const jobID = "job-deny"
	var gotChoice string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs/run_deny/approval" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Choice string `json:"choice"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotChoice = body.Choice
		_, _ = io.WriteString(w, `{"resolved":1}`)
	}))
	defer server.Close()

	fs := &fakeStore{jobs: map[string]store.Job{jobID: {ID: jobID, DisplayName: "Denied", Status: store.StatusRunning}}, permanent: map[string]bool{store.ApprovalPatternHash([]string{"@hermes-instance:" + server.URL, "recursive delete"}): true}}
	bus := events.NewBus(nil, testLog())
	h := NewHermes(server.URL, "", "hermes-agent", fs, bus, testLog())
	err := h.handleApprovalRequest(context.Background(), jobID, "run_deny", runEvent{
		Command: "rm -rf build", Description: "delete build", PatternKeys: []string{"recursive delete"},
	})
	if err != nil {
		t.Fatalf("handle approval: %v", err)
	}
	if gotChoice != "deny" {
		t.Fatalf("choice = %q, want deny", gotChoice)
	}
	j, _ := fs.ResolveJob(context.Background(), jobID)
	if j.Status != store.StatusRunning || len(j.Approvals) != 0 {
		t.Fatalf("auto-denied request became pending: %+v", j)
	}
}

func TestReconcileOrphansStopsAndCancelsRiverRetry(t *testing.T) {
	t.Parallel()
	var stopCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stop") {
			stopCalls++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	jobID := "job-restart"
	fs := &fakeStore{jobs: map[string]store.Job{jobID: {
		ID: jobID, Status: store.StatusNeedsApproval, HermesRunID: "run-restart",
		Approvals: []store.Approval{{ID: "approval", DecisionNonce: "nonce", State: "pending"}},
		Progress:  &store.Progress{Tool: "terminal", State: "running", Label: "editing config"},
	}}}
	h := NewHermes(srv.URL, "", "hermes-agent", fs, events.NewBus(nil, testLog()), testLog())
	if err := h.ReconcileOrphans(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, _ := fs.ResolveJob(context.Background(), jobID)
	if got.Status != store.StatusFailed || len(got.Approvals) != 0 || stopCalls != 1 {
		t.Fatalf("job=%+v stopCalls=%d", got, stopCalls)
	}
	if got.Progress == nil || got.Progress.Label != "editing config" {
		t.Fatalf("orphan reconciliation discarded progress evidence: %+v", got.Progress)
	}
	err := h.Run(context.Background(), jobID)
	var marker interface{ NoRetry() }
	if !errors.As(err, &marker) {
		t.Fatalf("retry error = %v, want no-retry marker", err)
	}
}

func TestRunResumesFromDurableHermesSessionAndVerificationBrief(t *testing.T) {
	t.Parallel()
	const jobID = "job-resume"
	var startBody struct {
		Input   string    `json:"input"`
		History []chatMsg `json:"conversation_history"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/sessions/"+jobID+"/messages":
			_, _ = io.WriteString(w, `{"object":"list","session_id":"job-resume","data":[{"role":"user","content":"original turn"},{"role":"assistant","content":"I changed one file before interruption"},{"role":"tool","content":"ignored tool payload"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			if err := json.NewDecoder(r.Body).Decode(&startBody); err != nil {
				t.Errorf("decode run start: %v", err)
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"run_id":"run-resumed","status":"started"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/run-resumed/events":
			_, _ = io.WriteString(w, "data: {\"event\":\"run.completed\",\"output\":\"resume complete\"}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	fs := &fakeStore{jobs: map[string]store.Job{jobID: {
		ID: jobID, DisplayName: "Resumable", Task: "finish the work", Status: store.StatusQueued,
		HermesSessionID: jobID, Error: "worker interrupted", Progress: &store.Progress{Tool: "terminal", State: "running", Label: "editing"},
	}}}
	h := NewHermes(srv.URL, "", "hermes-agent", fs, events.NewBus(nil, testLog()), testLog())
	if err := h.Run(context.Background(), jobID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(startBody.History) != 2 || startBody.History[0].Content != "original turn" || startBody.History[1].Content != "I changed one file before interruption" {
		t.Fatalf("recovered history = %#v", startBody.History)
	}
	for _, want := range []string{"finish the work", "worker interrupted", "Treat every previous side effect as indeterminate", "terminal / running / editing"} {
		if !strings.Contains(startBody.Input, want) {
			t.Errorf("resume input missing %q: %s", want, startBody.Input)
		}
	}
	j, _ := fs.ResolveJob(context.Background(), jobID)
	if j.Status != store.StatusDone || j.ResultDigest != "resume complete" || j.Error != "worker interrupted" {
		t.Fatalf("resumed job did not retain failure evidence: %+v", j)
	}
}

func TestLoadSessionHistoryKeepsNewestMessagesWithinByteBound(t *testing.T) {
	t.Parallel()
	messages := make([]map[string]string, 0, 13)
	for i := 0; i < 12; i++ {
		messages = append(messages, map[string]string{
			"role":    "user",
			"content": fmt.Sprintf("old-%02d-%s", i, strings.Repeat("x", 40<<10)),
		})
	}
	messages = append(messages, map[string]string{"role": "assistant", "content": "newest-critical-state"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": messages})
	}))
	defer srv.Close()

	h := NewHermes(srv.URL, "", "hermes-agent", nil, nil, testLog())
	history, err := h.loadSessionHistory(context.Background(), "job-history")
	if err != nil {
		t.Fatalf("loadSessionHistory: %v", err)
	}
	if len(history) == 0 || history[len(history)-1].Content != "newest-critical-state" {
		t.Fatalf("newest message was not retained: %#v", history)
	}
	if strings.HasPrefix(history[0].Content, "old-00-") {
		t.Fatalf("byte bound retained oldest rather than newest history: first=%q", history[0].Content[:16])
	}
}

func TestOrphanedApprovalAfterRestartStopsWithoutAllow(t *testing.T) {
	t.Parallel()
	var approvalCalls, stopCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/approval") {
			approvalCalls++
		}
		if strings.HasSuffix(r.URL.Path, "/stop") {
			stopCalls++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	jobID := "job-orphan"
	fs := &fakeStore{jobs: map[string]store.Job{jobID: {
		ID: jobID, DisplayName: "Orphan", Status: store.StatusNeedsApproval,
		HermesRunID: "run-orphan", Approvals: []store.Approval{{
			ID: "approval", DecisionNonce: "nonce", State: "pending",
		}},
	}}}
	h := NewHermes(srv.URL, "", "hermes-agent", fs, events.NewBus(nil, testLog()), testLog())
	if _, err := h.DecideApproval(context.Background(), jobID, "approval", "nonce", DecisionAllowOnce); err == nil || !strings.Contains(err.Error(), "no longer owned") {
		t.Fatalf("error = %v", err)
	}
	got, _ := fs.ResolveJob(context.Background(), jobID)
	if got.Status != store.StatusFailed || approvalCalls != 0 || stopCalls != 1 {
		t.Fatalf("job=%+v approvalCalls=%d stopCalls=%d", got, approvalCalls, stopCalls)
	}
}

func TestReusableAllowsStayExactSetAndHermesGetsOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		decision string
	}{
		{name: "session", decision: DecisionAllowSession},
		{name: "always", decision: DecisionAllowAlways},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var choice string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/approval") {
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					choice, _ = body["choice"].(string)
					_, _ = io.WriteString(w, `{"resolved":1}`)
					return
				}
				_, _ = io.WriteString(w, `{}`)
			}))
			defer srv.Close()

			jobID := "job-" + tc.name
			keys := []string{"A", "B"}
			fs := &fakeStore{jobs: map[string]store.Job{jobID: {
				ID: jobID, DisplayName: tc.name, Status: store.StatusNeedsApproval,
				HermesRunID: "run-" + tc.name, Approvals: []store.Approval{{
					ID: "approval", DecisionNonce: "nonce", State: "pending",
					PatternKeys: keys, AllowPermanent: true,
				}},
			}}}
			h := NewHermes(srv.URL, "", "hermes-agent", fs, events.NewBus(nil, testLog()), testLog())
			ownRun(h, jobID, "run-"+tc.name)
			if _, err := h.DecideApproval(context.Background(), jobID, "approval", "nonce", tc.decision); err != nil {
				t.Fatalf("decide: %v", err)
			}
			if choice != "once" {
				t.Fatalf("Hermes choice = %q, want once", choice)
			}
			if tc.decision == DecisionAllowSession {
				if h.matchesSessionAllow(jobID, keys) == "" || h.matchesSessionAllow(jobID, []string{"A"}) != "" {
					t.Fatal("session allow did not preserve exact-set scope")
				}
			} else {
				if got, _ := fs.MatchesPermanentAllow(context.Background(), h.persistentPolicyKeys(keys)); got == "" {
					t.Fatal("permanent exact-set allow was not saved")
				}
				if got, _ := fs.MatchesPermanentAllow(context.Background(), h.persistentPolicyKeys([]string{"A"})); got != "" {
					t.Fatal("permanent allow matched a subset")
				}
			}
		})
	}
}

func TestConcurrentDecisionReplaySendsOneAuthorityCall(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/approval") {
			mu.Lock()
			calls++
			if calls == 1 {
				close(entered)
			}
			mu.Unlock()
			<-release
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"resolved":1}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	jobID := "job-replay"
	fs := &fakeStore{jobs: map[string]store.Job{jobID: {
		ID: jobID, DisplayName: "Replay", Status: store.StatusNeedsApproval,
		HermesRunID: "run-replay", Approvals: []store.Approval{{
			ID: "approval", DecisionNonce: "nonce", State: "pending",
		}},
	}}}
	h := NewHermes(srv.URL, "", "hermes-agent", fs, events.NewBus(nil, testLog()), testLog())
	ownRun(h, "job-replay", "run-replay")
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := h.DecideApproval(context.Background(), jobID, "approval", "nonce", DecisionAllowOnce)
			errs <- err
		}()
	}
	<-entered
	close(release)
	var successes int
	for range 2 {
		if err := <-errs; err == nil {
			successes++
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 || successes != 1 {
		t.Fatalf("authority calls=%d successes=%d", calls, successes)
	}
}

func TestApprovalDecisionFailClosedOnIndeterminatePOST(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.URL.Path]++
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/approval") {
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	jobID := "job-indeterminate"
	fs := &fakeStore{jobs: map[string]store.Job{jobID: {
		ID: jobID, DisplayName: "Indeterminate", Status: store.StatusNeedsApproval,
		HermesRunID: "run-indeterminate", Approvals: []store.Approval{{
			ID: "approval-1", DecisionNonce: "nonce-1", State: "pending",
		}},
	}}}
	h := NewHermes(srv.URL, "", "hermes-agent", fs, events.NewBus(nil, testLog()), testLog())
	ownRun(h, "job-indeterminate", "run-indeterminate")
	_, err := h.DecideApproval(context.Background(), jobID, "approval-1", "nonce-1", DecisionAllowOnce)
	if err == nil || !strings.Contains(err.Error(), "indeterminate") {
		t.Fatalf("error = %v", err)
	}
	got, _ := fs.ResolveJob(context.Background(), jobID)
	if got.Status != store.StatusFailed || len(got.Approvals) != 1 || got.Approvals[0].State != "indeterminate" {
		t.Fatalf("job after indeterminate decision: %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["/v1/runs/run-indeterminate/approval"] != 1 || calls["/v1/runs/run-indeterminate/stop"] != 1 {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestSecondPendingApprovalDeniesAllAndStops(t *testing.T) {
	t.Parallel()
	var approvalBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/approval") {
			_ = json.NewDecoder(r.Body).Decode(&approvalBody)
			_, _ = io.WriteString(w, `{"resolved":2}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	jobID := "job-collision"
	fs := &fakeStore{jobs: map[string]store.Job{jobID: {
		ID: jobID, DisplayName: "Collision", Status: store.StatusNeedsApproval,
		HermesRunID: "run-collision", Approvals: []store.Approval{{
			ID: "first", DecisionNonce: "nonce", State: "pending",
		}},
	}}}
	h := NewHermes(srv.URL, "", "hermes-agent", fs, events.NewBus(nil, testLog()), testLog())
	err := h.handleApprovalRequest(context.Background(), jobID, "run-collision", runEvent{
		Event: "approval.request", Description: "second", PatternKeys: []string{"shell"},
	})
	if err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("error = %v", err)
	}
	got, _ := fs.ResolveJob(context.Background(), jobID)
	if got.Status != store.StatusFailed || len(got.Approvals) != 0 {
		t.Fatalf("job after collision: %+v", got)
	}
	if approvalBody["choice"] != "deny" || approvalBody["resolve_all"] != true {
		t.Fatalf("approval body = %#v", approvalBody)
	}
}

func TestPermanentAllowEligibilityAndNonceAreBrokerValidated(t *testing.T) {
	t.Parallel()
	jobID := "job-validate"
	fs := &fakeStore{jobs: map[string]store.Job{jobID: {
		ID: jobID, DisplayName: "Validate", Status: store.StatusNeedsApproval,
		HermesRunID: "run-validate", Approvals: []store.Approval{{
			ID: "approval", DecisionNonce: "nonce", State: "pending",
			PatternKeys: []string{"pattern"}, AllowPermanent: false,
		}},
	}}}
	h := NewHermes("http://127.0.0.1:1", "", "hermes-agent", fs, events.NewBus(nil, testLog()), testLog())
	ownRun(h, "job-validate", "run-validate")
	if _, err := h.DecideApproval(context.Background(), jobID, "approval", "stale", DecisionAllowOnce); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale nonce error = %v", err)
	}
	if _, err := h.DecideApproval(context.Background(), jobID, "approval", "nonce", DecisionAllowAlways); err == nil || !strings.Contains(err.Error(), "ineligible") {
		t.Fatalf("permanent allow error = %v", err)
	}
}

func TestInvalidApprovalDecisionFailsClosed(t *testing.T) {
	fs := &fakeStore{jobs: map[string]store.Job{"j": {ID: "j", Status: store.StatusNeedsApproval, Approvals: []store.Approval{{ID: "a", DecisionNonce: "nonce", State: "pending"}}}}, permanent: map[string]bool{}}
	h := NewHermes("http://invalid", "", "hermes-agent", fs, events.NewBus(nil, testLog()), testLog())
	if _, err := h.DecideApproval(context.Background(), "j", "a", "nonce", "yolo"); err == nil {
		t.Fatal("natural language reached authority boundary; wanted canonical-decision error")
	}
}

func TestSaferAlternativeDeniesConcreteMechanismAndRecordsIntent(t *testing.T) {
	var gotChoice string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runs/run-safer/approval" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Choice     string `json:"choice"`
			ResolveAll bool   `json:"resolve_all"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		gotChoice = body.Choice
		if body.ResolveAll {
			t.Fatal("safer-alternative decision must resolve only the correlated request")
		}
		_ = json.NewEncoder(w).Encode(map[string]int{"resolved": 1})
	}))
	defer srv.Close()

	jobID := "job-safer"
	fs := &fakeStore{jobs: map[string]store.Job{jobID: {
		ID: jobID, DisplayName: "Safer", Status: store.StatusNeedsApproval,
		HermesRunID: "run-safer", Approvals: []store.Approval{{
			ID: "approval", DecisionNonce: "nonce", State: "pending",
		}},
	}}, permanent: map[string]bool{}}
	h := NewHermes(srv.URL, "", "hermes-agent", fs, events.NewBus(nil, testLog()), testLog())
	h.mu.Lock()
	h.runIDs[jobID] = "run-safer"
	h.mu.Unlock()

	got, err := h.DecideApproval(context.Background(), jobID, "approval", "nonce", DecisionSaferAlternative)
	if err != nil {
		t.Fatal(err)
	}
	if gotChoice != "safer_alternative" {
		t.Fatalf("API choice = %q, want safer_alternative", gotChoice)
	}
	if got.Status != store.StatusRunning || len(got.Approvals) != 0 {
		t.Fatalf("job after safer alternative: %+v", got)
	}
	if got.Progress == nil || got.Progress.State != "safer_alternative" || !strings.Contains(got.Progress.Label, "safer") {
		t.Fatalf("safer-alternative intent not recorded: %+v", got.Progress)
	}
}

func TestSystemHeaderRequiresManagedOrDisposableDelegatedScripts(t *testing.T) {
	header := systemHeader(store.Job{DisplayName: "Script policy"})
	for _, required := range []string{
		"delegating its creation and debugging to a subagent",
		"reusable, named",
		"task-scoped temporary artifact removed",
		"Never leave behind an unexplained ad-hoc script",
	} {
		if !strings.Contains(header, required) {
			t.Fatalf("system header missing %q: %s", required, header)
		}
	}
}
