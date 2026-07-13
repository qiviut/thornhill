package events

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) { return len(p), nil }

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
