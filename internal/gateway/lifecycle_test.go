package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestShutdownCancelsAndWaitsForOwnedDesks(t *testing.T) {
	g := &Gateway{}
	deskCtx, cancelDesk := context.WithCancel(context.Background())
	g.deskStop = cancelDesk
	g.deskWG.Add(1)

	shortCtx, shortCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer shortCancel()
	if err := g.Shutdown(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown while Desk owned = %v, want deadline exceeded", err)
	}
	select {
	case <-deskCtx.Done():
	default:
		t.Fatal("Shutdown did not cancel active Desk")
	}

	g.deskWG.Done()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := g.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown after Desk exit: %v", err)
	}
	g.mu.Lock()
	closing := g.closing
	g.mu.Unlock()
	if !closing {
		t.Fatal("Shutdown did not retain new-Desk admission gate")
	}
	// The gate must return before consulting nil runtime dependencies.
	g.startDesk("rtc_after_shutdown")
}
