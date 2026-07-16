//go:build integration

package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func randomTestValue(t *testing.T, prefix string, bytes int) string {
	t.Helper()
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(raw)
}

func TestPostgresMigrationAndAtomicApprovalClaim(t *testing.T) {
	databaseURL := os.Getenv("THORNHILL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("THORNHILL_TEST_DATABASE_URL is required")
	}
	expectedSchema := os.Getenv("THORNHILL_TEST_SCHEMA")
	if expectedSchema == "" {
		t.Fatal("THORNHILL_TEST_SCHEMA is required")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	st, err := Open(ctx, databaseURL, log)
	if err != nil {
		t.Fatalf("first migration: %v", err)
	}
	st.Pool.Close()
	st, err = Open(ctx, databaseURL, log)
	if err != nil {
		t.Fatalf("idempotent migration: %v", err)
	}
	defer st.Pool.Close()

	var currentSchema string
	if err := st.Pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&currentSchema); err != nil || currentSchema != expectedSchema {
		t.Fatalf("current schema=%q want=%q err=%v", currentSchema, expectedSchema, err)
	}
	for _, table := range []string{"jobs", "approval_allows", "approval_denials", "deployment_control", "event_log", "summaries", "usage_ledger"} {
		var exists bool
		if err := st.Pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil || !exists {
			t.Fatalf("table %s exists=%v err=%v", table, exists, err)
		}
	}
	resumable, err := st.CreateJob(ctx, randomTestValue(t, "drain_resume_", 24), randomTestValue(t, "task_", 48))
	if err != nil {
		t.Fatal(err)
	}
	resumable, err = st.UpdateJob(ctx, resumable.ID, func(j *Job) { j.Status = StatusFailed })
	if err != nil {
		t.Fatal(err)
	}
	active, err := st.CreateJob(ctx, randomTestValue(t, "drain_active_", 24), randomTestValue(t, "task_", 48))
	if err != nil {
		t.Fatal(err)
	}
	active, err = st.UpdateJob(ctx, active.ID, func(j *Job) { j.Status = StatusRunning })
	if err != nil {
		t.Fatal(err)
	}
	activeApproval, err := st.CreateJob(ctx, randomTestValue(t, "active_approval_", 24), randomTestValue(t, "task_", 48))
	if err != nil {
		t.Fatal(err)
	}
	activeApproval, err = st.UpdateJob(ctx, activeApproval.ID, func(j *Job) { j.Status = StatusRunning })
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.Pool.Exec(ctx, `UPDATE deployment_control SET dispatch_paused=TRUE, updated_at=now() WHERE singleton=TRUE`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO jobs (id, display_name, task, status) VALUES ($1,$2,$3,$4)`,
		randomTestValue(t, "legacy_", 24), randomTestValue(t, "name_", 16), randomTestValue(t, "task_", 48), StatusQueued); err == nil {
		t.Fatal("direct legacy INSERT unexpectedly bypassed deployment drain trigger")
	}
	if _, err := st.CreateJob(ctx, randomTestValue(t, "paused_", 24), randomTestValue(t, "task_", 48)); !errors.Is(err, ErrDispatchPaused) {
		t.Fatalf("CreateJob while paused err=%v, want ErrDispatchPaused", err)
	}
	if _, err := st.UpdateJob(ctx, resumable.ID, func(j *Job) { j.Status = StatusQueued }); !errors.Is(err, ErrDispatchPaused) {
		t.Fatalf("resume transition while paused err=%v, want ErrDispatchPaused", err)
	}
	if _, err := st.UpdateJob(ctx, active.ID, func(j *Job) { j.Status = StatusNeedsInput }); err != nil {
		t.Fatalf("active job could not park for input while dispatch paused: %v", err)
	}
	if _, err := st.UpdateJob(ctx, activeApproval.ID, func(j *Job) { j.Status = StatusNeedsApproval }); err != nil {
		t.Fatalf("active job could not park for approval while dispatch paused: %v", err)
	}
	if _, err := st.UpdateJob(ctx, active.ID, func(j *Job) { j.Status = StatusDone }); err != nil {
		t.Fatalf("active job could not finish while dispatch paused: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE deployment_control SET dispatch_paused=FALSE, updated_at=now() WHERE singleton=TRUE`); err != nil {
		t.Fatal(err)
	}

	job, err := st.CreateJob(ctx, randomTestValue(t, "job_", 24), randomTestValue(t, "task_", 48))
	if err != nil {
		t.Fatal(err)
	}
	approvalID := randomTestValue(t, "approval_", 32)
	nonce := randomTestValue(t, "nonce_", 48)
	patterns := []string{randomTestValue(t, "pattern_", 20), randomTestValue(t, "pattern_", 20)}
	job, err = st.UpdateJob(ctx, job.ID, func(x *Job) {
		x.Status = StatusNeedsApproval
		x.HermesRunID = randomTestValue(t, "run_", 24)
		x.Approvals = []Approval{{ID: approvalID, DecisionNonce: nonce, State: "pending", PatternKeys: patterns}}
	})
	if err != nil {
		t.Fatal(err)
	}

	const contenders = 32
	var won, stale, other atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, claimErr := st.ClaimApproval(ctx, job.ID, approvalID, nonce)
			switch {
			case claimErr == nil:
				won.Add(1)
			case errors.Is(claimErr, ErrApprovalStale):
				stale.Add(1)
			default:
				other.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if won.Load() != 1 || stale.Load() != contenders-1 || other.Load() != 0 {
		t.Fatalf("claim results won=%d stale=%d other=%d", won.Load(), stale.Load(), other.Load())
	}
	claimed, err := st.ResolveJob(ctx, job.ID)
	if err != nil || len(claimed.Approvals) != 1 || claimed.Approvals[0].State != ApprovalStateSending {
		t.Fatalf("claimed state=%+v err=%v", claimed.Approvals, err)
	}

	parkJob, err := st.CreateJob(ctx, randomTestValue(t, "park_", 24), randomTestValue(t, "task_", 48))
	if err != nil {
		t.Fatal(err)
	}
	parkApprovalID := randomTestValue(t, "approval_", 32)
	parkNonce := randomTestValue(t, "nonce_", 48)
	parkJob, err = st.UpdateJob(ctx, parkJob.ID, func(x *Job) {
		x.Status = StatusNeedsApproval
		x.HermesRunID = randomTestValue(t, "run_", 24)
		x.Approvals = []Approval{{ID: parkApprovalID, DecisionNonce: parkNonce, State: ApprovalStatePending}}
	})
	if err != nil {
		t.Fatal(err)
	}
	var decisionWon, parkingWon, claimStale atomic.Int32
	startRace := make(chan struct{})
	var raceWG sync.WaitGroup
	raceWG.Add(2)
	go func() {
		defer raceWG.Done()
		<-startRace
		if _, claimErr := st.ClaimApproval(ctx, parkJob.ID, parkApprovalID, parkNonce); claimErr == nil {
			decisionWon.Add(1)
		} else if errors.Is(claimErr, ErrApprovalStale) {
			claimStale.Add(1)
		} else {
			other.Add(1)
		}
	}()
	go func() {
		defer raceWG.Done()
		<-startRace
		if _, parkErr := st.ParkApproval(ctx, parkJob.ID, parkApprovalID, parkNonce, "integration race", parkJob.UpdatedAt.Add(time.Second)); parkErr == nil {
			parkingWon.Add(1)
		} else if errors.Is(parkErr, ErrApprovalStale) {
			claimStale.Add(1)
		} else {
			other.Add(1)
		}
	}()
	close(startRace)
	raceWG.Wait()
	if decisionWon.Load()+parkingWon.Load() != 1 || claimStale.Load() != 1 || other.Load() != 0 {
		t.Fatalf("decision/parking race decision=%d parking=%d stale=%d other=%d",
			decisionWon.Load(), parkingWon.Load(), claimStale.Load(), other.Load())
	}
	raced, err := st.ResolveJob(ctx, parkJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parkingWon.Load() == 1 {
		if raced.Status != StatusParkedApproval || len(raced.Approvals) != 1 ||
			raced.Approvals[0].State != ApprovalStateParked || raced.Approvals[0].ParkedAt == nil {
			t.Fatalf("parking winner state=%+v", raced)
		}
	} else if raced.Status != StatusNeedsApproval || raced.Approvals[0].State != ApprovalStateSending {
		t.Fatalf("decision winner state=%+v", raced)
	}

	source := randomTestValue(t, "source_", 18)
	if err := st.SavePermanentAllows(ctx, patterns, source); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePermanentDenials(ctx, patterns, source); err != nil {
		t.Fatal(err)
	}
	if match, err := st.MatchesPermanentAllow(ctx, []string{patterns[1], "  " + patterns[0] + "  ", patterns[0]}); err != nil || match == "" {
		t.Fatalf("allow exact-set match=%q err=%v", match, err)
	}
	if match, err := st.MatchesPermanentDenial(ctx, []string{patterns[1], patterns[0]}); err != nil || match == "" {
		t.Fatalf("deny exact-set match=%q err=%v", match, err)
	}
	if match, _ := st.MatchesPermanentAllow(ctx, patterns[:1]); match != "" {
		t.Fatalf("subset unexpectedly matched allow policy: %q", match)
	}
}
