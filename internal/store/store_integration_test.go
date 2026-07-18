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
	for _, table := range []string{"jobs", "approval_allows", "approval_denials", "deployment_control", "event_log", "summaries", "usage_ledger", "attention_events", "push_subscriptions", "push_deliveries"} {
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

func TestPostgresAttentionClaimAckAndPushOutbox(t *testing.T) {
	databaseURL := os.Getenv("THORNHILL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("THORNHILL_TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := Open(ctx, databaseURL, log)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Pool.Close()
	// Other integration cases intentionally create terminal jobs. Isolate this
	// state-machine check from their pending operator-attention rows.
	if _, err := st.Pool.Exec(ctx, `UPDATE attention_events SET spoken_at=now(), claim_token='', claim_until=NULL WHERE spoken_at IS NULL`); err != nil {
		t.Fatal(err)
	}

	job, err := st.CreateJob(ctx, randomTestValue(t, "attention_", 24), randomTestValue(t, "task_", 48))
	if err != nil {
		t.Fatal(err)
	}
	job, err = st.UpdateJob(ctx, job.ID, func(j *Job) { j.Status = StatusRunning })
	if err != nil {
		t.Fatal(err)
	}
	job, err = st.UpdateJob(ctx, job.ID, func(j *Job) {
		j.Status = StatusDone
		j.ResultDigest = "sensitive integration result"
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.StateVersion != 2 {
		t.Fatalf("state version=%d want=2", job.StateVersion)
	}
	var attentionCount int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM attention_events WHERE job_id=$1`, job.ID).Scan(&attentionCount); err != nil || attentionCount != 1 {
		t.Fatalf("attention count=%d err=%v", attentionCount, err)
	}

	first, err := st.ClaimPendingAttention(ctx, "desk-one", 20, time.Minute)
	if err != nil || len(first) != 1 || first[0].JobID != job.ID {
		t.Fatalf("first claim=%+v err=%v", first, err)
	}
	second, err := st.ClaimPendingAttention(ctx, "desk-two", 20, time.Minute)
	if err != nil || len(second) != 0 {
		t.Fatalf("competing claim=%+v err=%v", second, err)
	}
	if marked, err := st.MarkAttentionSpoken(ctx, "desk-two", []int64{first[0].ID}); err != nil || marked != 0 {
		t.Fatalf("stale ack marked=%d err=%v", marked, err)
	}
	if err := st.ReleaseAttentionClaim(ctx, "desk-one"); err != nil {
		t.Fatal(err)
	}
	second, err = st.ClaimPendingAttention(ctx, "desk-two", 20, time.Minute)
	if err != nil || len(second) != 1 {
		t.Fatalf("released retry claim=%+v err=%v", second, err)
	}
	if marked, err := st.MarkAttentionSpoken(ctx, "desk-two", []int64{second[0].ID}); err != nil || marked != 1 {
		t.Fatalf("audible ack marked=%d err=%v", marked, err)
	}

	subscription := PushSubscription{
		Endpoint: "https://push.example.test/" + randomTestValue(t, "cap_", 24),
		P256DH:   randomTestValue(t, "key_", 32), Auth: randomTestValue(t, "auth_", 16),
	}
	if err := st.UpsertPushSubscription(ctx, subscription); err != nil {
		t.Fatal(err)
	}
	pushJob, err := st.CreateJob(ctx, randomTestValue(t, "push_", 24), randomTestValue(t, "task_", 48))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateJob(ctx, pushJob.ID, func(j *Job) { j.Status = StatusFailed }); err != nil {
		t.Fatal(err)
	}
	deliveries, err := st.ClaimPushDeliveries(ctx, "push-one", 20, time.Minute)
	if err != nil || len(deliveries) != 1 || deliveries[0].Endpoint != subscription.Endpoint {
		t.Fatalf("push claim=%+v err=%v", deliveries, err)
	}
	if duplicate, err := st.ClaimPushDeliveries(ctx, "push-two", 20, time.Minute); err != nil || len(duplicate) != 0 {
		t.Fatalf("duplicate push claim=%+v err=%v", duplicate, err)
	}
	retryAt := time.Now().UTC().Add(time.Minute)
	if err := st.MarkPushFailed(ctx, "push-one", deliveries[0].ID, retryAt, "transient"); err != nil {
		t.Fatal(err)
	}
	if retry, err := st.ClaimPushDeliveries(ctx, "push-two", 20, time.Minute); err != nil || len(retry) != 0 {
		t.Fatalf("premature retry=%+v err=%v", retry, err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE push_deliveries SET next_attempt_at=now() WHERE id=$1`, deliveries[0].ID); err != nil {
		t.Fatal(err)
	}
	retry, err := st.ClaimPushDeliveries(ctx, "push-two", 20, time.Minute)
	if err != nil || len(retry) != 1 || retry[0].Attempts < 2 {
		t.Fatalf("retry claim=%+v err=%v", retry, err)
	}
	if err := st.MarkPushDelivered(ctx, "push-two", retry[0].ID); err != nil {
		t.Fatal(err)
	}

	abandonedJob, err := st.CreateJob(ctx, randomTestValue(t, "abandoned_push_", 24), randomTestValue(t, "task_", 48))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateJob(ctx, abandonedJob.ID, func(j *Job) { j.Status = StatusFailed }); err != nil {
		t.Fatal(err)
	}
	abandoned, err := st.ClaimPushDeliveries(ctx, "push-abandoned", 20, time.Minute)
	if err != nil || len(abandoned) != 1 {
		t.Fatalf("abandoned push claim=%+v err=%v", abandoned, err)
	}
	if err := st.MarkPushAbandoned(ctx, "push-abandoned", abandoned[0].ID, "permanent rejection"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE push_deliveries SET next_attempt_at=now() WHERE id=$1`, abandoned[0].ID); err != nil {
		t.Fatal(err)
	}
	if replay, err := st.ClaimPushDeliveries(ctx, "push-after-abandon", 20, time.Minute); err != nil || len(replay) != 0 {
		t.Fatalf("abandoned delivery replay=%+v err=%v", replay, err)
	}
	var terminalFailure bool
	if err := st.Pool.QueryRow(ctx, `SELECT failed_at IS NOT NULL AND last_error <> '' FROM push_deliveries WHERE id=$1`, abandoned[0].ID).Scan(&terminalFailure); err != nil || !terminalFailure {
		t.Fatalf("terminal push failure recorded=%v err=%v", terminalFailure, err)
	}

	if err := st.DeletePushSubscription(ctx, subscription.Endpoint); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM push_subscriptions WHERE endpoint=$1`, subscription.Endpoint).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("unsubscribed rows=%d err=%v", remaining, err)
	}

	// Endpoint revocation reported by the push provider keeps a disabled audit
	// row, unlike an explicit operator unsubscribe.
	if err := st.UpsertPushSubscription(ctx, subscription); err != nil {
		t.Fatal(err)
	}
	var subscriptionID int64
	if err := st.Pool.QueryRow(ctx, `SELECT id FROM push_subscriptions WHERE endpoint=$1`, subscription.Endpoint).Scan(&subscriptionID); err != nil {
		t.Fatal(err)
	}
	if err := st.DisablePushSubscription(ctx, subscriptionID); err != nil {
		t.Fatal(err)
	}
	var disabled bool
	if err := st.Pool.QueryRow(ctx, `SELECT disabled_at IS NOT NULL FROM push_subscriptions WHERE id=$1`, subscriptionID).Scan(&disabled); err != nil || !disabled {
		t.Fatalf("subscription disabled=%v err=%v", disabled, err)
	}

	// A revoked endpoint can have an unsent delivery from just before the
	// provider response. Re-enrollment starts a fresh epoch and must not replay it.
	if err := st.UpsertPushSubscription(ctx, subscription); err != nil {
		t.Fatal(err)
	}
	staleJob, err := st.CreateJob(ctx, randomTestValue(t, "stale_push_", 24), randomTestValue(t, "task_", 48))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateJob(ctx, staleJob.ID, func(j *Job) { j.Status = StatusFailed }); err != nil {
		t.Fatal(err)
	}
	staleDeliveries, err := st.ClaimPushDeliveries(ctx, "push-stale", 20, time.Minute)
	if err != nil || len(staleDeliveries) != 1 {
		t.Fatalf("stale delivery materialization=%+v err=%v", staleDeliveries, err)
	}
	if err := st.ReleasePushClaim(ctx, "push-stale"); err != nil {
		t.Fatal(err)
	}
	if err := st.DisablePushSubscription(ctx, subscriptionID); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPushSubscription(ctx, subscription); err != nil {
		t.Fatal(err)
	}
	if replay, err := st.ClaimPushDeliveries(ctx, "push-reactivated", 20, time.Minute); err != nil || len(replay) != 0 {
		t.Fatalf("reactivation replay=%+v err=%v", replay, err)
	}
}
