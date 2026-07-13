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
	// Answer forwards operator input to a job in needs_input.
	Answer(ctx context.Context, jobID, text string) error
	// DecideApproval resolves the oldest pending authority request using one
	// of Thornhill's typed allow/deny/safer-alternative decisions.
	DecideApproval(ctx context.Context, jobID, approvalID, nonce, decision string) (store.Job, error)
	// Cancel best-effort aborts a running job's agent work.
	Cancel(ctx context.Context, jobID string)
}

// Queue abstracts River so the dispatcher is testable without Postgres.
type Queue interface {
	EnqueueRun(ctx context.Context, jobID string) error
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
	j, err := d.store.CreateJob(ctx, name, task)
	if err != nil {
		return store.Job{}, err
	}
	if err := d.queue.EnqueueRun(ctx, j.ID); err != nil {
		_, _ = d.store.UpdateJob(ctx, j.ID, func(x *store.Job) {
			x.Status = store.StatusFailed
			x.Error = "enqueue failed: " + err.Error()
		})
		return store.Job{}, fmt.Errorf("enqueue: %w", err)
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
	if j.Status == store.StatusDone || j.Status == store.StatusFailed || j.Status == store.StatusCancelled {
		return j, fmt.Errorf("job %q already %s", j.DisplayName, j.Status)
	}
	d.runner.Cancel(ctx, j.ID)
	j, err = d.store.UpdateJob(ctx, j.ID, func(x *store.Job) { x.Status = store.StatusCancelled })
	if err != nil {
		return store.Job{}, err
	}
	d.bus.Publish(events.KindJobCancelled, j.ID, j)
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
	if err := d.runner.Answer(ctx, j.ID, text); err != nil {
		return store.Job{}, err
	}
	j, err = d.store.UpdateJob(ctx, j.ID, func(x *store.Job) {
		x.Status = store.StatusRunning
		x.Question = ""
	})
	if err != nil {
		return store.Job{}, err
	}
	d.bus.Publish(events.KindJobRunning, j.ID, j)
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
	if j.Status != store.StatusFailed {
		return j, fmt.Errorf("job %q is %s; only failed jobs can be resumed", j.DisplayName, j.Status)
	}
	// Stop any persisted upstream run before clearing its identity. Hermes.Cancel
	// falls back to the durable run ID after a Thornhill restart.
	d.runner.Cancel(ctx, j.ID)
	claimed := false
	j, err = d.store.UpdateJob(ctx, j.ID, func(x *store.Job) {
		claimed = prepareForResume(x)
	})
	if err != nil {
		return store.Job{}, err
	}
	if !claimed {
		return j, fmt.Errorf("job %q is %s; another resume or state transition won the race", j.DisplayName, j.Status)
	}
	if err := d.queue.EnqueueRun(ctx, j.ID); err != nil {
		failed, _ := d.store.UpdateJob(ctx, j.ID, func(x *store.Job) {
			x.Status = store.StatusFailed
			x.Error = "resume enqueue failed after: " + x.Error + "; " + err.Error()
		})
		return failed, fmt.Errorf("resume enqueue: %w", err)
	}
	d.bus.Publish(events.KindJobQueued, j.ID, j)
	d.log.Info("failed job queued for safe resume", "id", j.ID, "name", j.DisplayName)
	return j, nil
}

func prepareForResume(x *store.Job) bool {
	if x.Status != store.StatusFailed {
		return false
	}
	x.Status = store.StatusQueued
	x.Question = ""
	x.ResultDigest = ""
	x.HermesRunID = ""
	x.Approvals = nil
	x.FinishedAt = nil
	// Preserve Error and Progress until Hermes snapshots them into the resume
	// verification brief. They are evidence about interrupted side effects, not
	// ephemeral UI state.
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
	// A pending approval or long autonomous run has no elapsed decision/runtime
	// deadline. Explicit cancel, shutdown, and worker failure still cancel ctx.
	return -1
}
