//go:build integration

package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"thornhill/internal/events"
	"thornhill/internal/store"
)

type transactionalTestQueue struct {
	fail bool
}

func (q *transactionalTestQueue) EnqueueRunTx(ctx context.Context, tx pgx.Tx, jobID string) error {
	if q.fail {
		return errors.New("synthetic enqueue failure")
	}
	_, err := tx.Exec(ctx, `INSERT INTO dispatch_test_enqueues (job_id) VALUES ($1)`, jobID)
	return err
}

type integrationRunner struct {
	mu             sync.Mutex
	releaseErr     error
	releasedRunIDs []string
	cancelCalls    int
}

func (r *integrationRunner) Run(context.Context, string) error { return nil }

func (r *integrationRunner) DecideApproval(context.Context, string, string, string, string) (store.Job, error) {
	return store.Job{}, errors.New("not implemented in integration runner")
}

func (r *integrationRunner) Cancel(context.Context, string) {
	r.mu.Lock()
	r.cancelCalls++
	r.mu.Unlock()
}

func (r *integrationRunner) ReleaseRun(_ context.Context, _, runID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releasedRunIDs = append(r.releasedRunIDs, runID)
	return r.releaseErr
}

func integrationDispatcher(t *testing.T, q Queue, r Runner) (*Dispatcher, *store.Store) {
	t.Helper()
	databaseURL := os.Getenv("THORNHILL_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("THORNHILL_TEST_DATABASE_URL is required")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(context.Background(), databaseURL, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Pool.Close)
	if _, err := st.Pool.Exec(context.Background(), `
		CREATE TABLE IF NOT EXISTS dispatch_test_enqueues (job_id TEXT PRIMARY KEY);
		TRUNCATE dispatch_test_enqueues`); err != nil {
		t.Fatal(err)
	}
	return New(st, events.NewBus(nil, log), q, r, log), st
}

func TestDispatchAndResumeCommitWithQueueDelivery(t *testing.T) {
	ctx := context.Background()
	runner := &integrationRunner{}
	q := &transactionalTestQueue{fail: true}
	d, st := integrationDispatcher(t, q, runner)
	name := "rollback-" + store.NewULID()

	if _, err := d.Dispatch(ctx, name, "must roll back"); err == nil {
		t.Fatal("dispatch unexpectedly succeeded with failing transactional queue")
	}
	var jobs, deliveries int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE display_name=$1`, name).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM dispatch_test_enqueues`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 || deliveries != 0 {
		t.Fatalf("failed dispatch committed jobs=%d deliveries=%d", jobs, deliveries)
	}

	q.fail = false
	created, err := d.Dispatch(ctx, "atomic-"+store.NewULID(), "commit together")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM dispatch_test_enqueues WHERE job_id=$1`, created.ID).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 {
		t.Fatalf("committed delivery count=%d, want 1", deliveries)
	}

	failed, err := st.CreateJob(ctx, "resume-"+store.NewULID(), "preserve failure")
	if err != nil {
		t.Fatal(err)
	}
	failed, err = st.UpdateJob(ctx, failed.ID, func(j *store.Job) {
		j.Status = store.StatusFailed
		j.Error = "durable failure evidence"
	})
	if err != nil {
		t.Fatal(err)
	}
	q.fail = true
	if _, err := d.Resume(ctx, failed.ID); err == nil {
		t.Fatal("resume unexpectedly succeeded with failing transactional queue")
	}
	after, err := st.ResolveJob(ctx, failed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != store.StatusFailed || after.Error != failed.Error || after.FinishedAt == nil {
		t.Fatalf("failed resume did not roll back exactly: %+v", after)
	}
}

func TestCancelledDeliveryAndDurableAnswerCannotResurrectTerminalState(t *testing.T) {
	ctx := context.Background()
	runner := &integrationRunner{}
	d, st := integrationDispatcher(t, &transactionalTestQueue{}, runner)

	cancelled, err := st.CreateJob(ctx, "cancel-"+store.NewULID(), "never execute")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err = st.UpdateJob(ctx, cancelled.ID, func(j *store.Job) { j.Status = store.StatusCancelled })
	if err != nil {
		t.Fatal(err)
	}
	stub := NewStubRunner(st, events.NewBus(nil, slog.New(slog.NewTextHandler(io.Discard, nil))), time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := stub.Run(ctx, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	afterCancel, err := st.ResolveJob(ctx, cancelled.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterCancel.Status != store.StatusCancelled {
		t.Fatalf("cancelled delivery resurrected as %s", afterCancel.Status)
	}

	waiting, err := st.CreateJob(ctx, "answer-"+store.NewULID(), "complete immediately")
	if err != nil {
		t.Fatal(err)
	}
	waiting, err = st.UpdateJob(ctx, waiting.ID, func(j *store.Job) {
		j.Status = store.StatusNeedsInput
		j.Question = "proceed?"
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Answer(ctx, waiting.ID, "yes"); err != nil {
		t.Fatal(err)
	}
	answered, err := st.ResolveJob(ctx, waiting.ID)
	if err != nil {
		t.Fatal(err)
	}
	if answered.Status != store.StatusQueued || answered.PendingInput != "yes" || answered.Question != "" {
		t.Fatalf("answer was not durably queued: %+v", answered)
	}
}

func TestCompletionWinningRowLockCannotBeOverwrittenByCancel(t *testing.T) {
	ctx := context.Background()
	runner := &integrationRunner{}
	d, st := integrationDispatcher(t, &transactionalTestQueue{}, runner)
	j, err := st.CreateJob(ctx, "cancel-race-"+store.NewULID(), "finish first")
	if err != nil {
		t.Fatal(err)
	}
	j, err = st.UpdateJob(ctx, j.ID, func(x *store.Job) { x.Status = store.StatusRunning })
	if err != nil {
		t.Fatal(err)
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	completion := make(chan error, 1)
	go func() {
		_, updateErr := st.UpdateJob(ctx, j.ID, func(x *store.Job) {
			close(locked)
			<-release
			x.Status = store.StatusDone
		})
		completion <- updateErr
	}()
	<-locked
	cancelResult := make(chan error, 1)
	go func() {
		_, cancelErr := d.Cancel(ctx, j.ID)
		cancelResult <- cancelErr
	}()
	close(release)
	if err := <-completion; err != nil {
		t.Fatal(err)
	}
	if err := <-cancelResult; err == nil {
		t.Fatal("cancel unexpectedly overwrote completed job")
	}
	got, err := st.ResolveJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusDone {
		t.Fatalf("completion was overwritten by %s", got.Status)
	}
}

func TestResumeRetainsCleanupHandleWhenExactRunReleaseFails(t *testing.T) {
	ctx := context.Background()
	runner := &integrationRunner{releaseErr: errors.New("stop unavailable")}
	d, st := integrationDispatcher(t, &transactionalTestQueue{}, runner)
	j, err := st.CreateJob(ctx, "parked-"+store.NewULID(), "resume safely")
	if err != nil {
		t.Fatal(err)
	}
	j, err = st.UpdateJob(ctx, j.ID, func(x *store.Job) {
		x.Status = store.StatusParkedApproval
		x.HermesRunID = "old-run"
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Resume(ctx, j.ID); err == nil {
		t.Fatal("resume succeeded despite unconfirmed old-run stop")
	}
	got, err := st.ResolveJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusParkedApproval || got.HermesRunID != "old-run" {
		t.Fatalf("failed release discarded cleanup obligation: %+v", got)
	}
}

func TestConcurrentResumeNeverCancelsReplacementRun(t *testing.T) {
	ctx := context.Background()
	runner := &integrationRunner{}
	d, st := integrationDispatcher(t, &transactionalTestQueue{}, runner)
	j, err := st.CreateJob(ctx, "resume-race-"+store.NewULID(), "resume once")
	if err != nil {
		t.Fatal(err)
	}
	j, err = st.UpdateJob(ctx, j.ID, func(x *store.Job) {
		x.Status = store.StatusParkedApproval
		x.HermesRunID = "captured-old-run"
	})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, resumeErr := d.Resume(ctx, j.ID)
			results <- resumeErr
		}()
	}
	close(start)
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful resumes=%d, want 1", successes)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.cancelCalls != 0 {
		t.Fatalf("resume called generic Cancel %d times", runner.cancelCalls)
	}
	for _, runID := range runner.releasedRunIDs {
		if runID != "captured-old-run" {
			t.Fatalf("resume released fresh run %q", runID)
		}
	}
}
