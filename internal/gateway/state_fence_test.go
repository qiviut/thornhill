package gateway

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"thornhill/internal/desk"
	"thornhill/internal/events"
)

type blockingPersister struct {
	entered chan struct{}
	release chan struct{}
}

func (p *blockingPersister) AppendEvent(context.Context, events.Event) error {
	select {
	case <-p.entered:
	default:
		close(p.entered)
	}
	<-p.release
	return nil
}

func TestDeskStatePublicationFencesSupersededDeskAtomically(t *testing.T) {
	persist := &blockingPersister{entered: make(chan struct{}), release: make(chan struct{})}
	bus := events.NewBus(persist, slog.New(slog.NewTextHandler(io.Discard, nil)))
	oldDesk, newDesk := &desk.Desk{}, &desk.Desk{}
	g := &Gateway{Bus: bus, current: oldDesk}

	published := make(chan struct{})
	go func() {
		g.publishDeskState(oldDesk, desk.StateParked, "old")
		close(published)
	}()
	<-persist.entered

	attempting := make(chan struct{})
	replaced := make(chan struct{})
	go func() {
		close(attempting)
		g.mu.Lock()
		g.current = newDesk
		g.mu.Unlock()
		close(replaced)
	}()
	<-attempting
	select {
	case <-replaced:
		t.Fatal("replacement passed the current-desk fence before publication completed")
	case <-time.After(25 * time.Millisecond):
	}

	close(persist.release)
	<-published
	<-replaced

	// Once superseded, the old Desk cannot append another terminal state.
	g.publishDeskState(oldDesk, desk.StateParked, "stale")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	replay := bus.Subscribe(ctx, true)
	select {
	case e := <-replay:
		if e.Kind != events.KindSessionState {
			t.Fatalf("kind = %q", e.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("missing first state event")
	}
	select {
	case e := <-replay:
		t.Fatalf("stale state event escaped fence: %+v", e)
	case <-time.After(25 * time.Millisecond):
	}
}
