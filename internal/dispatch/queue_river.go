package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"thornhill/internal/events"
	"thornhill/internal/store"
)

// RiverQueue is the production Queue: durable, retrying, leased.
type RiverQueue struct {
	Client *river.Client[pgx.Tx]
}

func (q *RiverQueue) EnqueueRun(ctx context.Context, jobID string) error {
	_, err := q.Client.Insert(ctx, RunArgs{JobID: jobID}, nil)
	return err
}

// StartRiver migrates River's schema and starts a working client with the
// hermes_run worker registered.
func StartRiver(ctx context.Context, pool *pgxpool.Pool, runner Runner, log *slog.Logger) (*RiverQueue, func(context.Context) error, error) {
	driver := riverpgxv5.New(pool)
	migrator, err := rivermigrate.New(driver, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return nil, nil, fmt.Errorf("river migrate: %w", err)
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, &Worker{Runner: runner, Log: log})

	client, err := river.NewClient(driver, &river.Config{
		Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 4}},
		Workers: workers,
		// Agent jobs are expensive and conversational; a blind retry storm
		// against Hermes helps nobody. One retry for transients, then park
		// the failure on the board.
		MaxAttempts: 2,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("river client: %w", err)
	}
	if err := client.Start(ctx); err != nil {
		return nil, nil, fmt.Errorf("river start: %w", err)
	}
	log.Info("river started", "max_workers", 4)
	return &RiverQueue{Client: client}, client.Stop, nil
}

// --- stub runner ---

// StubRunner fakes a slow Hermes job: runs for the configured duration,
// asks one clarifying question halfway if the task contains a '?', then
// completes. Lets the entire voice loop be exercised with only an OpenAI
// key and no fleet.
type StubRunner struct {
	Store    *store.Store
	Bus      *events.Bus
	Duration time.Duration
	Log      *slog.Logger

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	answers map[string]chan string
}

func NewStubRunner(st *store.Store, bus *events.Bus, dur time.Duration, log *slog.Logger) *StubRunner {
	return &StubRunner{Store: st, Bus: bus, Duration: dur, Log: log,
		cancels: map[string]context.CancelFunc{}, answers: map[string]chan string{}}
}

func (s *StubRunner) Run(ctx context.Context, jobID string) error {
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancels[jobID] = cancel
	ans := make(chan string, 1)
	s.answers[jobID] = ans
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.cancels, jobID)
		delete(s.answers, jobID)
		s.mu.Unlock()
	}()

	j, err := s.Store.UpdateJob(ctx, jobID, func(x *store.Job) { x.Status = store.StatusRunning })
	if err != nil {
		return err
	}
	s.Bus.Publish(events.KindJobRunning, jobID, j)
	s.Log.Info("stub job running", "id", jobID, "for", s.Duration)

	half := s.Duration / 2
	select {
	case <-ctx.Done():
		return nil // cancelled via Cancel(); status already set by dispatcher
	case <-time.After(half):
	}

	// One synthetic clarifying question exercises the needs_input bridge.
	j, err = s.Store.UpdateJob(ctx, jobID, func(x *store.Job) {
		x.Status = store.StatusNeedsInput
		x.Question = "Stub checkpoint: proceed with the default approach?"
	})
	if err != nil {
		return err
	}
	s.Bus.Publish(events.KindJobNeedsInput, jobID, j)

	select {
	case <-ctx.Done():
		return nil
	case a := <-ans:
		s.Log.Info("stub job got answer", "id", jobID, "answer", a)
	case <-time.After(30 * time.Minute):
		// Operator never answered; finish anyway so the stub can't wedge.
	}

	select {
	case <-ctx.Done():
		return nil
	case <-time.After(half):
	}

	j, err = s.Store.UpdateJob(ctx, jobID, func(x *store.Job) {
		x.Status = store.StatusDone
		x.ResultDigest = "Stub job completed after " + s.Duration.String() + ". (Wire HERMES_BASE_URL for the real thing.)"
	})
	if err != nil {
		return err
	}
	s.Bus.Publish(events.KindJobDone, jobID, j)
	return nil
}

func (s *StubRunner) Answer(_ context.Context, jobID, text string) error {
	s.mu.Lock()
	ch, ok := s.answers[jobID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("job not waiting in this process (restart lost the in-memory conversation)")
	}
	select {
	case ch <- text:
	default:
	}
	return nil
}

func (s *StubRunner) DecideApproval(ctx context.Context, jobID, _, _, _ string) (store.Job, error) {
	j, err := s.Store.ResolveJob(ctx, jobID)
	if err != nil {
		return store.Job{}, err
	}
	return j, fmt.Errorf("stub runner has no pending Hermes approval")
}

func (s *StubRunner) Cancel(_ context.Context, jobID string) {
	s.mu.Lock()
	if c, ok := s.cancels[jobID]; ok {
		c()
	}
	s.mu.Unlock()
}
