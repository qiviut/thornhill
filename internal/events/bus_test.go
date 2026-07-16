package events

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) { return len(p), nil }

type blockingPersister struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	events  []Event
}

func (p *blockingPersister) AppendEvent(ctx context.Context, e Event) error {
	p.once.Do(func() { close(p.entered) })
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.Lock()
	p.events = append(p.events, e)
	p.mu.Unlock()
	return nil
}

func TestPublishFanoutAndReplay(t *testing.T) {
	b := NewBus(nil, testLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	early := b.Subscribe(ctx, false)
	b.Publish(KindJobQueued, "j1", map[string]string{"display_name": "alpha"})
	b.Publish(KindJobDone, "j1", nil)

	// Early subscriber sees both live.
	for _, want := range []string{KindJobQueued, KindJobDone} {
		select {
		case e := <-early:
			if e.Kind != want {
				t.Fatalf("want %s got %s", want, e.Kind)
			}
		case <-time.After(time.Second):
			t.Fatalf("timeout waiting for %s", want)
		}
	}

	// Late subscriber with replay sees the ring.
	late := b.Subscribe(ctx, true)
	var got []string
	deadline := time.After(time.Second)
	for len(got) < 2 {
		select {
		case e := <-late:
			got = append(got, e.Kind)
		case <-deadline:
			t.Fatalf("replay timeout, got %v", got)
		}
	}
	if got[0] != KindJobQueued || got[1] != KindJobDone {
		t.Fatalf("replay order wrong: %v", got)
	}
}

func TestSeqMonotonic(t *testing.T) {
	b := NewBus(nil, testLog())
	e1 := b.Publish(KindUsage, "", nil)
	e2 := b.Publish(KindUsage, "", nil)
	if e2.Seq <= e1.Seq {
		t.Fatalf("seq not monotonic: %d then %d", e1.Seq, e2.Seq)
	}
}

func TestLaggingSubscriberDoesNotBlockPublish(t *testing.T) {
	b := NewBus(nil, testLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = b.Subscribe(ctx, false) // never drained

	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ { // > channel buffer, must not block
			b.Publish(KindUsage, "", nil)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a lagging subscriber")
	}
}

func TestPublishDoesNotWaitForPersistenceAndCloseDrainsInOrder(t *testing.T) {
	p := &blockingPersister{entered: make(chan struct{}), release: make(chan struct{})}
	b := NewBus(p, testLog())
	published := make(chan struct{})
	go func() {
		b.Publish(KindJobQueued, "j1", nil)
		b.Publish(KindJobRunning, "j1", nil)
		close(published)
	}()
	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("Publish waited for blocked persistence")
	}
	select {
	case <-p.entered:
	case <-time.After(time.Second):
		t.Fatal("persistence worker did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- b.Close(context.Background()) }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before queued persistence drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(p.release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.events) != 2 || p.events[0].Kind != KindJobQueued || p.events[1].Kind != KindJobRunning {
		t.Fatalf("persisted events out of order: %+v", p.events)
	}
}

func TestSynchronousPublishWaitsBehindEarlierQueuedEvents(t *testing.T) {
	p := &blockingPersister{entered: make(chan struct{}), release: make(chan struct{})}
	b := NewBus(p, testLog())
	b.Publish(KindJobQueued, "j1", nil)
	select {
	case <-p.entered:
	case <-time.After(time.Second):
		t.Fatal("persistence worker did not start")
	}
	syncDone := make(chan struct{})
	go func() {
		b.PublishSync(KindSessionState, "", map[string]string{"state": "LIVE"})
		close(syncDone)
	}()
	select {
	case <-syncDone:
		t.Fatal("synchronous publish returned before earlier persistence completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(p.release)
	select {
	case <-syncDone:
	case <-time.After(time.Second):
		t.Fatal("synchronous publish did not receive persistence acknowledgement")
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.events) != 2 || p.events[0].Kind != KindJobQueued || p.events[1].Kind != KindSessionState {
		t.Fatalf("mixed persistence order = %+v", p.events)
	}
}

func TestUnsubscribeOnCancel(t *testing.T) {
	b := NewBus(nil, testLog())
	ctx, cancel := context.WithCancel(context.Background())
	ch := b.Subscribe(ctx, false)
	cancel()
	b.Publish(KindUsage, "", nil) // nudge the pump goroutine to observe cancel
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // closed as expected
			}
		case <-deadline:
			t.Fatal("subscriber channel not closed after cancel")
		}
	}
}
