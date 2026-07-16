// Package dispatch owns job lifecycle and the durable queue (River).
// A job is a conversation with a Hermes session; the dispatcher only
// tracks orchestration state — knowledge lives Hermes-side (Obsidian,
// beads, Curator). With HERMES_BASE_URL unset a stub worker fakes a
// slow job so the whole voice loop can be exercised with only an
// OpenAI key.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"thornhill/internal/events"
	"thornhill/internal/store"
)

// Runner executes a job against Hermes (or the stub). Implemented by
// bridge.Hermes and StubRunner.
type Runner interface {
	// Run drives the job to a terminal state or needs_input, updating the
	// store and publishing events as it goes.
	Run(ctx context.Context, jobID string) error
	// DecideApproval resolves the oldest pending authority request using one
	// of Thornhill's typed allow/deny/safer-alternative decisions.
	DecideApproval(ctx context.Context, jobID, approvalID, nonce, decision string) (store.Job, error)
	// ReleaseRun confirms that one captured upstream run stopped. It must never
	// retarget a newer execution for the same Thornhill job.
	ReleaseRun(ctx context.Context, jobID, runID string) error
	// Cancel best-effort aborts a running job's agent work.
	Cancel(ctx context.Context, jobID string)
}

// Queue abstracts River so the dispatcher is testable without Postgres.
type Queue interface {
	EnqueueRunTx(ctx context.Context, tx pgx.Tx, jobID string) error
}

type Dispatcher struct {
	store  *store.Store
	bus    *events.Bus
	queue  Queue
	runner Runner
	log    *slog.Logger
}

func New(st *store.Store, bus *events.Bus, q Queue, r Runner, log *slog.Logger) *Dispatcher {
	return &Dispatcher{store: st, bus: bus, queue: q, runner: r, log: log}
}

func (d *Dispatcher) Dispatch(ctx context.Context, name, task string) (store.Job, error) {
	tx, err := d.store.Pool.Begin(ctx)
	if err != nil {
		return store.Job{}, err
	}
	defer tx.Rollback(ctx)
	j, err := d.store.CreateJobTx(ctx, tx, name, task)
	if err != nil {
		return store.Job{}, err
	}
	if err := d.queue.EnqueueRunTx(ctx, tx, j.ID); err != nil {
		return store.Job{}, fmt.Errorf("enqueue: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return store.Job{}, fmt.Errorf("commit dispatch: %w", err)
	}
	d.bus.Publish(events.KindJobQueued, j.ID, j)
	d.log.Info("job dispatched", "id", j.ID, "name", name)
	return j, nil
}

func (d *Dispatcher) Resolve(ctx context.Context, ref string) (store.Job, error) {
	return d.store.ResolveJob(ctx, ref)
}

func (d *Dispatcher) Active(ctx context.Context) ([]store.Job, error) {
	return d.store.ActiveJobs(ctx)
}

func (d *Dispatcher) Cancel(ctx context.Context, ref string) (store.Job, error) {
	j, err := d.store.ResolveJob(ctx, ref)
	if err != nil {
		return store.Job{}, err
	}
	cancelled := false
	j, err = d.store.UpdateJob(ctx, j.ID, func(x *store.Job) {
		if x.Status != store.StatusDone && x.Status != store.StatusFailed && x.Status != store.StatusCancelled {
			x.Status = store.StatusCancelled
			cancelled = true
		}
	})
	if err != nil {
		return store.Job{}, err
	}
	if !cancelled {
		return j, fmt.Errorf("job %q already %s", j.DisplayName, j.Status)
	}
	d.bus.Publish(events.KindJobCancelled, j.ID, j)
	// Persisted cancellation is authoritative. The runner signal is best effort;
	// stale workers are separately fenced from writing any later state.
	d.runner.Cancel(ctx, j.ID)
	return j, nil
}

func (d *Dispatcher) Answer(ctx context.Context, ref, text string) (store.Job, error) {
	j, err := d.store.ResolveJob(ctx, ref)
	if err != nil {
		return store.Job{}, err
	}
	if j.Status != store.StatusNeedsInput {
		return j, fmt.Errorf("job %q is %s, not waiting for input", j.DisplayName, j.Status)
	}
	tx, err := d.store.Pool.Begin(ctx)
	if err != nil {
		return j, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	claimed := false
	j, err = d.store.UpdateJobTx(ctx, tx, j.ID, func(x *store.Job) {
		if x.Status == store.StatusNeedsInput {
			x.Status = store.StatusQueued
			x.Question = ""
			x.PendingInput = text
			claimed = true
		}
	})
	if err != nil {
		return store.Job{}, err
	}
	if !claimed {
		return j, fmt.Errorf("job %s is %s, not waiting for input", j.DisplayName, j.Status)
	}
	if err := d.queue.EnqueueRunTx(ctx, tx, j.ID); err != nil {
		return j, fmt.Errorf("enqueue answered job: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return j, err
	}
	d.bus.Publish(events.KindJobQueued, j.ID, j)
	return j, nil
}

func (d *Dispatcher) DecideApproval(ctx context.Context, ref, approvalID, nonce, decision string) (store.Job, error) {
	j, err := d.store.ResolveJob(ctx, ref)
	if err != nil {
		return store.Job{}, err
	}
	if j.Status != store.StatusNeedsApproval || len(j.Approvals) == 0 {
		return j, fmt.Errorf("job %q is %s, not waiting for approval", j.DisplayName, j.Status)
	}
	return d.runner.DecideApproval(ctx, j.ID, approvalID, nonce, decision)
}

func (d *Dispatcher) Resume(ctx context.Context, ref string) (store.Job, error) {
	j, err := d.store.ResolveJob(ctx, ref)
	if err != nil {
		return store.Job{}, err
	}
	if j.Status != store.StatusFailed && j.Status != store.StatusParkedApproval {
		return j, fmt.Errorf("job %q is %s; only failed or parked-approval jobs can be resumed", j.DisplayName, j.Status)
	}
	wasParked := j.Status == store.StatusParkedApproval
	expectedStatus := j.Status
	expectedRunID := j.HermesRunID
	if expectedRunID != "" {
		if err := d.runner.ReleaseRun(ctx, j.ID, expectedRunID); err != nil {
			return j, fmt.Errorf("release prior Hermes run %s: %w", expectedRunID, err)
		}
	}
	tx, err := d.store.Pool.Begin(ctx)
	if err != nil {
		return store.Job{}, err
	}
	defer tx.Rollback(ctx)
	claimed := false
	j, err = d.store.UpdateJobTx(ctx, tx, j.ID, func(x *store.Job) {
		if x.Status == expectedStatus && x.HermesRunID == expectedRunID {
			claimed = prepareForResume(x)
		}
	})
	if err != nil {
		return store.Job{}, err
	}
	if !claimed {
		return j, fmt.Errorf("job %q is %s; another resume or state transition won the race", j.DisplayName, j.Status)
	}
	if err := d.queue.EnqueueRunTx(ctx, tx, j.ID); err != nil {
		return j, fmt.Errorf("resume enqueue: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return j, fmt.Errorf("commit resume: %w", err)
	}
	d.bus.Publish(events.KindJobQueued, j.ID, j)
	d.log.Info("job queued for safe resume", "id", j.ID, "name", j.DisplayName, "from_parked_approval", wasParked)
	return j, nil
}

func prepareForResume(x *store.Job) bool {
	previous := x.Status
	if previous != store.StatusFailed && previous != store.StatusParkedApproval {
		return false
	}
	x.Status = store.StatusQueued
	x.Question = ""
	x.ResultDigest = ""
	x.HermesRunID = ""
	if previous == store.StatusFailed {
		x.Approvals = nil
	}
	x.FinishedAt = nil
	// Preserve Error and Progress until Hermes snapshots them into the resume
	// verification brief. A parked approval also remains attached until Run
	// snapshots its untrusted evidence, then it is cleared before a new run starts.
	return true
}

func (d *Dispatcher) Rename(ctx context.Context, ref, newName string) (store.Job, error) {
	j, err := d.store.ResolveJob(ctx, ref)
	if err != nil {
		return store.Job{}, err
	}
	old := j.DisplayName
	j, err = d.store.UpdateJob(ctx, j.ID, func(x *store.Job) { x.DisplayName = newName })
	if err != nil {
		return store.Job{}, err
	}
	d.bus.Publish(events.KindJobRenamed, j.ID, map[string]string{"from": old, "to": newName})
	return j, nil
}

// --- River integration ---

type RunArgs struct {
	JobID string `json:"job_id"`
}

func (RunArgs) Kind() string { return "hermes_run" }

type Worker struct {
	river.WorkerDefaults[RunArgs]
	Runner Runner
	Log    *slog.Logger
}

func (w *Worker) Work(ctx context.Context, job *river.Job[RunArgs]) error {
	w.Log.Info("worker picked job", "job_id", job.Args.JobID, "attempt", job.Attempt)
	err := w.Runner.Run(ctx, job.Args.JobID)
	var noRetry interface{ NoRetry() }
	if errors.As(err, &noRetry) {
		return river.JobCancel(err)
	}
	return err
}

func (w *Worker) Timeout(*river.Job[RunArgs]) time.Duration {
	// River itself has no elapsed decision/runtime deadline. Approval parking
	// reclaims the worker after its separately configured threshold without
	// granting or denying authority; explicit cancel and shutdown still cancel ctx.
	return -1
}
